# Description
Proxies packets from server to client and back. Allows to buffer them for both client and server side, so client and server wont miss a packet. Simulates streaming connection using http requests.

#### Authorization
1. Client sends `POST /auth` or `POST /register` request with binary data in body required to server to authenticate client
1. Server receives this data and should return client id and token for successfully authorised/registered client
1. Client should use this token for every packet request 

#### Client checks for awaiting packets and sends packet to server
1. Client sends a packet to the proxy `POST /`
1. Proxy saves packet to the server buffer and keeps long-polled connection for some time
1. Proxy tries to send the packet to one of servers. If it's unreachable - it tries another server or repeats delayed
1. If there is a packet for the client - proxy responses with a packet

#### Additional headers
Any headers starting with `X-` are transferred from client to server and vice versa. 

#### Configuration
Run with --help parameter to get information about configuration.

#### Debugging
1. Debug server authorises with `Accept-Debug` permission and fetch packets time to time as a client
2. Client sends packets to proxy with header `Debug` with value of debug server client



# API


### Endpoints


#### POST /auth
Authentication request.
###### Request headers
|Header|Description
|---|---
|Permissions|Optional comma separated permission codes to login with
###### Request body
Any data required to the server to process authorization. Will be passed through unchanged
###### Result codes
|Code|Description
|---|---
|200|Success
|401|Client is not authorized. Invalid login or password
|403|Client is not authorized. Permissions are not granted
###### Response body
Body of the response contains UTF-8 encoded access token.


#### POST /register
Register new user request.
###### Request headers
Same as `POST /auth`
###### Request body
Any data required to the server to process registration. Will be passed through unchanged
###### Result codes
Same as `POST /auth`
###### Response body
Same as `POST /auth`



#### POST /
Buffers a packet to the server and returns oldest packet waiting.
###### Request body
Packet bytes. If there is no packet - remain body empty.
###### Request headers
|Header|Description
|---|---
|Authorization|Access token in format `Bearer XXX` where XXX is a token string|
|[To]|Client id of the debug server (optional)
###### Result codes
|Code|Description
|---|---
|200|Success. Body of the response contains packet or nothing if there is no packet awaiting
|401|Client is not authorized or token is expired
###### Response headers
|Header|Description
|---|---
|More|Returns number of packets awaiting to be fetched
|[From]|Id of client or server that sent this packet (for debug client working as server only)
###### Response body
Oldest stored packet bytes. If there were no packets – body will be empty.



### Permission codes
|Code|Description
|---|---
|Accept-Debug|Marks client as debug server allowing to redirect request to this client instead of server



### Server endpoint implementation
Server should implement one endpoint. It will receive different kind of data, depending on request type. Proxy sends only `POST` requests. Server may use long-polling for requests.

|Request Headers|Request body|Result Codes|Response Headers|Response Body
|---|---|---|---|---
|`Type`=`Auth`, `Permissions`=...|Binary data from client|Same as `POST /auth`, will be proxied|`Client-Id`=...|Access token (UTF-8)|
|`Type`=`Register`, `Permissions`=...|Binary data from client|Same as `POST /register`, will be proxied|`Client-Id`=...|Access token (UTF-8)|
|`Type`=`Packet`, `From`=*Client-Id*|Binary packet or empty to fetch awaiting packets.|200|`To`=*Client-id*|Binary packet. If there is no packet - response body should be empty and `To` header omitted.
