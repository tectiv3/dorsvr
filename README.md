Dorsvr Streaming Server
=======================
[![GitHub issues](https://img.shields.io/github/issues/tectiv3/dorsvr.svg)](https://github.com/tectiv3/dorsvr/issues)

## Modules
 * rtspserver - rtsp server
 * rtspclient - rtsp client
 * groupsock  - group socket
 * livemedia  - media library

## Feature
 * Streaming Video (H264, M2TS)
 * Streaming Audio (MP3)
 * Protocols: RTP, RTCP, RTSP
 * Access Control

## Install
    go get github.com/tectiv3/dorsvr

## Format
    $ make fmt

## Testing
    $ make test

## Example
```golang
import (
    "fmt"

    "github.com/tectiv3/dorsvr/rtspserver"
)

func main() {
    server := rtspserver.New(nil)

    portNum := 8554
    err := server.Listen(portNum)
    if err != nil {
        fmt.Printf("Failed to bind port: %d\n", portNum)
        return
    }

    if !server.SetupTunnelingOverHTTP(80) ||
        !server.SetupTunnelingOverHTTP(8000) ||
        !server.SetupTunnelingOverHTTP(8080) {
        fmt.Printf("We use port %d for optional RTSP-over-HTTP tunneling, "+
                   "or for HTTP live streaming (for indexed Transport Stream files only).\n", server.HTTPServerPortNum())
    } else {
        fmt.Println("(RTSP-over-HTTP tunneling is not available.)")
    }

    urlPrefix := server.RtspURLPrefix()
    fmt.Println("This server's URL: " + urlPrefix + "<filename>.")

    server.Start()

    select {}
}
```
## Author
djwackey, worcy_kiddy@126.com

## LICENSE
dorsvr is licensed under the GNU Lesser General Public License, Version 2.1. See [LICENSE](https://github.com/tectiv3/dorsvr/blob/master/LICENSE) for the full license text.
