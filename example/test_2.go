package main

import (
	"fmt"
	"github.com/mofadeyunduo/go-socket.io-client"
	"log"
	"time"
)

type Register struct {
	PeerId string `json:"peerId"`
}

type RegisterRsp struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func main() {
	opts := &socketio_client.Options{
		Transport: "websocket",
		Query:     map[string]string{"EIO": "3"},
	}
	uri := "http://localhost:3001"
	client, err := socketio_client.NewClient(uri, opts)
	if err != nil {
		log.Printf("NewClient error:%v\n", err)
		return
	}
	client.On("keepaliveRsp", func(msg RegisterRsp, err error) {
		fmt.Println(msg, "-------", err)
	})
	r := Register{PeerId: "111"}
	client.Emit("keepalive", &r)
	time.Sleep(time.Second)
	//client.Close()
	time.Sleep(time.Hour)
}
