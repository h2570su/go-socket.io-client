* This fork based on [mofadeyunduo/go-socket.io-client](https://github.com/mofadeyunduo/go-socket.io-client)

And merged commits from original branch [hesh915/go-socket.io-client](https://github.com/hesh915/go-socket.io-client)
From commits [4a28084](https://github.com/hesh915/go-socket.io-client/commit/4a28084e51bbaf224ce345a91c8740c626b7a77e) to [83ee737](https://github.com/hesh915/go-socket.io-client/commit/83ee73793ba4ec64250ecd10639dc203663a1cb8)

Then modified a little bit, added a bugfix on onAck read map failed
# go-socket.io-client

go-socket.io-client is an client implementation of [socket.io](http://socket.io) in golang, which is a realtime application framework.

It is compatible with latest implementation of socket.io in node.js, and supports namespace.

* It is base on [googollee/go-socket.io](https://github.com/googollee/go-socket.io) and [googollee/go-engine.io](https://github.com/googollee/go-engine.io)

## Install

Install the package with:

```bash
go get github.com/h2570su/go-socket.io-client
```

Import it with:

```go
import "github.com/h2570su/go-socket.io-client"
```

and use `socketio_client` as the package name inside the code.

## Example

Please check the example folder for details.

```go
package main

import (
	"bufio"
	"github.com/h2570su/go-socket.io-client"
	"log"
	"os"
)

func main() {

	opts := &socketio_client.Options{
		Transport: "websocket",
		Query:     make(map[string]string),
	}
	opts.Query["user"] = "user"
	opts.Query["pwd"] = "pass"
	uri := "http://192.168.1.70:9090/socket.io/"

	client, err := socketio_client.NewClient(uri, opts)
	if err != nil {
		log.Printf("NewClient error:%v\n", err)
		return
	}

	client.On("error", func() {
		log.Printf("on error\n")
	})
	client.On("connection", func() {
		log.Printf("on connect\n")
	})
	client.On("message", func(msg string) {
		log.Printf("on message:%v\n", msg)
	})
	client.On("disconnection", func() {
		log.Printf("on disconnect\n")
	})

	reader := bufio.NewReader(os.Stdin)
	for {
		data, _, _ := reader.ReadLine()
		command := string(data)
		client.Emit("message", command)
		log.Printf("send message:%v\n", command)
	}
}
```

## License

The 3-clause BSD License  - see LICENSE for more details
