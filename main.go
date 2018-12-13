// Copyright 2018 Sergey Dvoynikov. All rights reserved.
// Use of this source code is governed by the MIT license that can be found in the LICENSE file.

package main

import (
	"bytes"
	"context"
	"flag"
	"fmt"
	"github.com/gin-gonic/gin"
	"github.com/sirupsen/logrus"
	"github.com/snowzach/rotatefilehook"
	"github.com/toorop/gin-logrus"
	"github.com/zbindenren/logrus_mail"
	"golang.org/x/net/netutil"
	"io/ioutil"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

var (
	debug, logAsText                           bool
	logFile                                    string
	logMaxSize, logMaxAge, logMaxBackups       int
	email, smtpAddress, smtpUser, smtpPassword string

	addr               string
	timeout            time.Duration
	longPollingTimeout time.Duration
	maxClients         int
	keepAlivePeriod    time.Duration

	backendUrl        string
	backendBufferSize int
	backendThreads    int
	bufferSize        int

	tokenTTL time.Duration
)

var (
	log        *logrus.Logger
	httpServer *http.Server
	httpClient *http.Client
	backend    *server

	clients      = map[string]*client{}
	tokensLookup = map[string]string{}
	clientsLock  = &sync.RWMutex{}
)

func main() {
	defer func() {
		if err := recover(); err != nil {
			if log != nil {
				log.Fatalf("%v", err)
			} else {
				fmt.Printf("%v", err)
			}
		}
	}()

	parseFlags()
	if err := setupLog(); err != nil {
		log.WithError(err).Fatal("cannot configure logger properly: %v", err)
	}

	setupRouter()
	setupBackend()

	if err := serve(); err != nil {
		log.WithError(err).Fatal("error while serving http")
	}
}

func parseFlags() {
	flag.StringVar(&addr, "addr", ":80", "TCP address to listen on")
	flag.DurationVar(&timeout, "timeout", 20*time.Second, "Read/write socket timeout")
	flag.DurationVar(&longPollingTimeout, "longPollingTimeout", 10*time.Second, "Long polling timeout. Should be less than timeout")
	flag.IntVar(&maxClients, "maxClients", 2048, "Maximum simultaneous connections")
	flag.DurationVar(&keepAlivePeriod, "keepAlivePeriod", 3*time.Minute, "Tcp connection keepalive, O for disable")

	flag.StringVar(&backendUrl, "backendUrl", "http://localhost:801", "Backend server url")
	flag.IntVar(&backendBufferSize, "backendBufferSize", 4096, "Amount of packets to store for backend")
	flag.IntVar(&backendThreads, "backendThreads", 32, "Amount of parallel threads sending requests to backend")
	flag.IntVar(&bufferSize, "bufferSize", 16, "Amount of packets to store per client")

	flag.DurationVar(&tokenTTL, "tokenTTL", 24*time.Hour, "Access token expiration time")

	flag.BoolVar(&debug, "debug", false, "Redirects log to stdout and enables debug mode for router")
	flag.BoolVar(&logAsText, "logAsText", false, "Enables text instead of json logger")
	flag.StringVar(&logFile, "logFile", "./logs/full.log", "Path to log file")
	flag.IntVar(&logMaxSize, "logMaxSize", 100, "Max size of log in MB")
	flag.IntVar(&logMaxAge, "logMaxAge", 0, "Max age of log files in days. Default is 0 (files are not removed using this criteria)")
	flag.IntVar(&logMaxBackups, "logMaxBackups", 0, "Max number of log backup files. Default is 0 (files are not removed using this criteria)")
	flag.StringVar(&email, "email", "", "Email to send errors to. Default is empty (no emails will be sent)")
	flag.StringVar(&smtpAddress, "smtpAddress", "", "smtp address in format server:port")
	flag.StringVar(&smtpUser, "smtpUser", "", "Username for smtp server")
	flag.StringVar(&smtpPassword, "smtpPassword", "", "Password for smtp server")

	flag.Parse()
}

func setupLog() error {
	var err error
	var hook logrus.Hook

	log = logrus.New()
	if debug {
		log.SetOutput(os.Stdout)
		log.SetLevel(logrus.DebugLevel)
	} else {
		var formatter logrus.Formatter
		if logAsText {
			formatter = &logrus.TextFormatter{}
		} else {
			formatter = &logrus.JSONFormatter{}
		}

		hook, err = rotatefilehook.NewRotateFileHook(rotatefilehook.RotateFileConfig{
			Filename:   logFile,
			MaxSize:    logMaxSize,
			MaxBackups: logMaxBackups,
			MaxAge:     logMaxAge,
			Level:      logrus.WarnLevel,
			Formatter:  formatter,
		})

		if err != nil {
			return err
		}
	}

	if email != "" && smtpAddress != "" {
		smtp := strings.Split(smtpAddress, ":")
		if len(smtp) != 2 {
			return fmt.Errorf("smtp address string is incorrect (%v)", smtpAddress)
		}
		var port int
		port, err = strconv.Atoi(smtp[1])
		if err != nil {
			return fmt.Errorf("smtp address string is incorrect (%v)", smtpAddress)
		}

		if smtpUser != "" {
			hook, err = logrus_mail.NewMailAuthHook("BufferedProxy", smtp[0], port, email, email, smtpUser, smtpPassword)
		} else {
			hook, err = logrus_mail.NewMailHook("BufferedProxy", smtp[0], port, email, email)
		}

		if err != nil {
			return err
		}

		log.Hooks.Add(hook)
	}
	return nil
}

func setupRouter() {
	var engine *gin.Engine
	if debug {
		engine = gin.Default()
	} else {
		gin.SetMode(gin.ReleaseMode)
		engine = gin.New()
		engine.Use(ginlogrus.Logger(log), gin.Recovery())
	}

	engine.POST("/auth", createAuthHandler("Auth"))
	engine.POST("/register", createAuthHandler("Register"))
	engine.POST("/", servePacket)

	httpServer = &http.Server{
		Addr:           addr,
		Handler:        engine,
		ReadTimeout:    timeout,
		WriteTimeout:   timeout,
		MaxHeaderBytes: 1 << 15,
	}

	httpClient = &http.Client{
		Timeout: timeout,
	}
}

func setupBackend() {
	backend = &server{
		Buffer: make(chan *packet, backendBufferSize),
	}

	interrupt := make(chan bool)

	go func() {
		var gracefulStop = make(chan os.Signal)
		signal.Notify(gracefulStop, syscall.SIGTERM)
		signal.Notify(gracefulStop, syscall.SIGINT)

		<-gracefulStop
		for i := 0; i < backendThreads+2; i++ {
			interrupt <- true
		}
	}()

	for i := 0; i < backendThreads; i++ {
		go backend.watch(interrupt)
	}

	go ticker(interrupt)
	go expirator(interrupt)
}

func ticker(interrupt chan bool) {
	if err := recover(); err != nil {
		log.Errorf("watcher error: %+v", err)
		go ticker(interrupt)
	}
	for true {
		if len(backend.Buffer) == 0 {
			h := http.Header{}
			h.Set("Type", "Packet")
			backend.sendBuffered(&packet{Header: h})
		}

		select {
		case <-time.After(longPollingTimeout):
			break
		case <-interrupt:
			return
		}
	}
}

func expirator(interrupt chan bool) {
	if err := recover(); err != nil {
		log.Errorf("watcher error: %+v", err)
		go expirator(interrupt)
	}

	for true {
		select {
		case <-time.After(time.Minute):
			deadline := time.Now().Add(-tokenTTL)
			var timedOut []*client
			clientsLock.RLock()
			for _, c := range clients {
				if c.LastActivity.Before(deadline) {
					timedOut = append(timedOut, c)
				}
			}
			clientsLock.RUnlock()

			if len(timedOut) > 0 {
				clientsLock.Lock()
				for _, c := range timedOut {
					delete(tokensLookup, c.Token)
					delete(clients, c.ClientId)
				}
				clientsLock.Unlock()
			}
		case <-interrupt:
			return
		}
	}
}

func serve() error {
	go func() {
		var gracefulStop = make(chan os.Signal)
		signal.Notify(gracefulStop, syscall.SIGTERM)
		signal.Notify(gracefulStop, syscall.SIGINT)

		s := <-gracefulStop
		log.Tracef("shutdown signal received: %v", s)

		if err := httpServer.Shutdown(context.Background()); err != nil {
			log.WithError(err).Fatal("error while shutting server down")
		}
	}()

	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return err
	}

	err = httpServer.Serve(netutil.LimitListener(tcpKeepAliveListener{ln.(*net.TCPListener)}, maxClients))
	if err == http.ErrServerClosed {
		err = nil
	}

	return err
}

func createAuthHandler(requestType string) gin.HandlerFunc {
	return func(c *gin.Context) {

		body, err := ioutil.ReadAll(c.Request.Body)
		if err != nil {
			c.AbortWithStatus(http.StatusBadRequest)
			return
		}

		p := &packet{
			Header: extractHeaders(c.Request.Header, "Type", requestType, "Permissions", c.GetHeader("Permissions")),
			Data:   body,
		}

		res, err := p.send()
		if err != nil {
			log.WithError(err).Errorf("cannot proxy /auth request")
			c.AbortWithStatus(http.StatusInternalServerError)
			return
		}

		tokenBytes, err := ioutil.ReadAll(res.Body)
		if err != nil {
			log.WithError(err).Errorf("cannot read auth response body")
			c.AbortWithStatus(http.StatusInternalServerError)
			return
		}

		if err = res.Body.Close(); err != nil {
			log.WithError(err).Warnf("cannot close response body")
		}

		if res.StatusCode == http.StatusOK {
			clientId := res.Header.Get("Client-Id")
			if clientId == "" {
				log.WithError(err).Errorf("backend server returned empty Client-Id for successful auth response")
				c.AbortWithStatus(http.StatusInternalServerError)
				return
			}

			if err != nil || len(tokenBytes) == 0 {
				log.WithError(err).Errorf("backend server returned empty token for successful auth response")
				c.AbortWithStatus(http.StatusInternalServerError)
				return
			}
			token := string(tokenBytes)

			clientsLock.Lock()
			tokensLookup[token] = clientId
			client := clients[clientId]
			if client == nil {
				client = newClient(clientId, token)
				clients[clientId] = client
			} else {
				delete(tokensLookup, client.Token)
				client.Token = token
			}
			clientsLock.Unlock()

			client.AcceptDebug = c.GetHeader("Accept-Debug") != ""
			for k, vv := range extractHeaders(res.Header) {
				for _, v := range vv {
					c.Header(k, v)
				}
			}
		}

		c.Status(res.StatusCode)
		_, err = c.Writer.Write(tokenBytes)
	}
}

func servePacket(c *gin.Context) {
	auth := c.GetHeader("Authorization")
	token := strings.TrimPrefix(auth, "Bearer ")

	clientsLock.RLock()
	clientId := tokensLookup[token]
	client := clients[clientId]
	clientsLock.RUnlock()

	if client == nil {
		c.AbortWithStatus(http.StatusUnauthorized)
		return
	}

	body, err := ioutil.ReadAll(c.Request.Body)
	if err != nil {
		log.WithError(err).Errorf("cannot read packet request body")
		c.AbortWithStatus(http.StatusInternalServerError)
		return
	}

	if len(body) > 0 {
		p := &packet{
			Header: extractHeaders(c.Request.Header, "Type", "Packet", "From", client.ClientId),
			Data:   body,
		}

		to := c.GetHeader("To")
		if to == "" {
			backend.sendBuffered(p)
		} else {
			clientsLock.RLock()
			client := clients[to]
			clientsLock.Unlock()

			if client == nil || !client.AcceptDebug {
				log.Errorf("trying to send packet to non-debug client")
			} else {
				client.Buffer <- p
			}
		}
	}

	select {
	case <-time.After(longPollingTimeout):
		c.Header("More", "0")
		c.Status(http.StatusOK)
		break
	case p := <-client.Buffer:
		c.Header("More", strconv.Itoa(len(client.Buffer)))
		for k, vv := range p.Header {
			for _, v := range vv {
				c.Header(k, v)
			}
		}
		c.Status(http.StatusOK)
		_, err = c.Writer.Write(p.Data)
		if err != nil {
			client.BufferWriteLock.Lock()
			buffer := make(chan *packet, bufferSize)
			buffer <- p
			for len(buffer) < cap(buffer) && len(client.Buffer) > 0 {
				buffer <- <-client.Buffer
				client.Buffer = buffer
			}
			client.BufferWriteLock.Unlock()

			log.WithError(err).Errorf("cannot write packet response body")
		}
		break
	}
}

func extractHeaders(header http.Header, more ...string) http.Header {
	res := http.Header{}

	for k, v := range header {
		if strings.HasPrefix(k, "X-") {
			res[strings.TrimPrefix(k, "X-")] = v
		}
	}

	if len(more) > 1 {
		for i, v := range more {
			if i%2 == 1 {
				res.Add(more[i-1], v)
			}
		}
	}

	return res
}

type server struct {
	Buffer chan *packet
}

func (s *server) sendBuffered(p *packet) {
	s.Buffer <- p
}

func (s *server) watch(interrupt chan bool) {
	defer func() {
		if err := recover(); err != nil {
			log.Errorf("watcher error: %+v", err)
			go s.watch(interrupt)
		}
	}()
	for true {
		select {
		case p := <-s.Buffer:
			for true {
				res, err := p.send()

				if res == nil && err == nil {
					log.Tracef("packet redirected to debug client (%v bytes)", len(p.Data))
					break
				}

				if err != nil || res.StatusCode != 200 {
					if err != nil {
						log.WithError(err).Errorf("cannot send request to backend server")
					} else {
						log.Errorf("backend server does not respond with code %v", res.StatusCode)
					}

					if len(s.Buffer) == cap(s.Buffer) {
						log.Warnf("dropping packet for backend server (%v bytes)", len(p.Data))
						break
					}

					<-time.After(longPollingTimeout)
					continue
				}

				clientId := res.Header.Get("To")
				if clientId != "" {

					clientsLock.Lock()
					client := clients[clientId]
					if client == nil {
						client = newClient(clientId, "")
						clients[clientId] = client
					}
					clientsLock.Unlock()

					if len(client.Buffer) == cap(client.Buffer) {
						dropped := <-client.Buffer
						log.Tracef("dropping packet for %v (%v bytes)", client.ClientId, len(dropped.Data))
					}

					data, err := ioutil.ReadAll(res.Body)
					if err != nil {
						log.WithError(err).Error("cannot fetch packet from backend")
						continue
					}

					if err = res.Body.Close(); err != nil {
						log.WithError(err).Warnf("cannot close response body")
					}

					revp := &packet{
						Header: extractHeaders(res.Header),
						Data:   data,
					}
					client.Buffer <- revp
				}
				break
			}
			break
		case <-interrupt:
			return
		}
	}
}

type client struct {
	ClientId        string
	Token           string
	LastActivity    time.Time
	Buffer          chan *packet
	BufferWriteLock sync.Locker
	AcceptDebug     bool
}

func newClient(id, token string) *client {
	return &client{
		ClientId:        id,
		Token:           token,
		LastActivity:    time.Now(),
		Buffer:          make(chan *packet, bufferSize),
		BufferWriteLock: &sync.Mutex{},
	}
}

type packet struct {
	Header http.Header
	Data   []byte
}

func (p *packet) send() (*http.Response, error) {
	req, err := http.NewRequest(http.MethodPost, backendUrl, bytes.NewReader(p.Data))
	if err != nil {
		return nil, err
	}

	if p.Header != nil {
		req.Header = p.Header
	}

	return httpClient.Do(req)
}

type tcpKeepAliveListener struct {
	*net.TCPListener
}

func (ln tcpKeepAliveListener) Accept() (net.Conn, error) {
	tc, err := ln.AcceptTCP()
	if err != nil {
		return nil, err
	}
	if keepAlivePeriod == 0 {
		if err := tc.SetKeepAlive(false); err != nil {
			log.WithError(err).Errorf("Failed to switch off keep alive")
		}
	} else {
		if err := tc.SetKeepAlive(true); err != nil {
			log.WithError(err).Errorf("Failed to switch on keep alive")
		}
		if err := tc.SetKeepAlivePeriod(keepAlivePeriod); err != nil {
			log.WithError(err).Errorf("Failed to set keep alive period")
		}
	}
	return tc, nil
}
