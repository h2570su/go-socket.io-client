package socketio_client

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	log "github.com/sirupsen/logrus"
	"github.com/zhouhui8915/engine.io-go/message"
	"github.com/zhouhui8915/engine.io-go/parser"
	"github.com/zhouhui8915/engine.io-go/polling"
	"github.com/zhouhui8915/engine.io-go/transport"
	"github.com/zhouhui8915/engine.io-go/websocket"
)

var InvalidError = errors.New("invalid transport")

var transports = []string{"polling", "websocket"}

var creators map[string]transport.Creater

func init() {
	creators = make(map[string]transport.Creater)
	for _, t := range transports {
		switch t {
		case "polling":
			creators[t] = polling.Creater
		case "websocket":
			creators[t] = websocket.Creater
		}
	}
}

type MessageType message.MessageType

const (
	MessageBinary MessageType = MessageType(message.MessageBinary)
	MessageText   MessageType = MessageType(message.MessageText)
)

type state int

const (
	stateUnknown state = iota
	stateNormal
	stateUpgrading
	stateClosing
	stateClosed
)

type clientConn struct {
	id              string
	options         *Options
	url             *url.URL
	request         *http.Request
	writerLocker    sync.Mutex
	transportLocker sync.RWMutex
	currentName     string
	current         transport.Client
	upgradingName   string
	upgrading       transport.Client
	state           state
	stateLocker     sync.RWMutex
	readerChan      chan *connReader
	pingTimeout     time.Duration
	pingInterval    time.Duration
	pingChan        chan bool
}

func newClientConn(opts *Options, u *url.URL) (client *clientConn, err error) {
	if opts.Transport == nil {
		opts.Transport = []string{"websocket", "polling"}
	}

	for _, transport := range opts.Transport {
		_, exists := creators[transport]
		if !exists {
			return nil, InvalidError
		}
	}

	client = &clientConn{
		url:          u,
		options:      opts,
		state:        stateNormal,
		pingTimeout:  60000 * time.Millisecond,
		pingInterval: 25000 * time.Millisecond,
		pingChan:     make(chan bool),
		readerChan:   make(chan *connReader),
	}

	err = client.onOpen()
	if err != nil {
		return
	}

	go client.pingLoop()
	go client.readLoop()

	return
}

func (c *clientConn) Id() string {
	return c.id
}

func (c *clientConn) Request() *http.Request {
	return c.request
}

func (c *clientConn) NextReader() (MessageType, io.ReadCloser, error) {
	if c.getState() == stateClosed {
		return MessageBinary, nil, io.EOF
	}
	ret := <-c.readerChan
	if ret == nil {
		return MessageBinary, nil, io.EOF
	}
	return MessageType(ret.MessageType()), ret, nil
}

func (c *clientConn) NextWriter(t MessageType) (io.WriteCloser, error) {
	switch c.getState() {
	case stateUpgrading:
		for i := 0; i < 30; i++ {
			time.Sleep(50 * time.Millisecond)
			if c.getState() != stateUpgrading {
				break
			}
		}
		if c.getState() == stateUpgrading {
			return nil, fmt.Errorf("upgrading")
		}
	case stateNormal:
	default:
		return nil, io.EOF
	}
	c.writerLocker.Lock()
	ret, err := c.getCurrent().NextWriter(message.MessageType(t), parser.MESSAGE)
	if err != nil {
		c.writerLocker.Unlock()
		return ret, err
	}
	writer := newConnWriter(ret, &c.writerLocker)
	return writer, err
}

func (c *clientConn) Close() error {
	if c.getState() != stateNormal && c.getState() != stateUpgrading {
		return nil
	}
	if c.upgrading != nil {
		c.upgrading.Close()
	}
	c.writerLocker.Lock()
	if w, err := c.getCurrent().NextWriter(message.MessageText, parser.CLOSE); err == nil {
		writer := newConnWriter(w, &c.writerLocker)
		writer.Close()
	} else {
		c.writerLocker.Unlock()
	}
	if err := c.getCurrent().Close(); err != nil {
		return err
	}
	c.setState(stateClosing)
	return nil
}

func (c *clientConn) OnPacket(r *parser.PacketDecoder) {
	if s := c.getState(); s != stateNormal && s != stateUpgrading {
		return
	}
	switch r.Type() {
	case parser.OPEN:
	case parser.CLOSE:
		c.getCurrent().Close()
	case parser.PING:
		t := c.getCurrent()
		u := c.getUpgrade()
		newWriter := t.NextWriter
		c.writerLocker.Lock()
		if u != nil {
			if w, _ := t.NextWriter(message.MessageText, parser.NOOP); w != nil {
				w.Close()
			}
			newWriter = u.NextWriter
		}
		if w, _ := newWriter(message.MessageText, parser.PONG); w != nil {
			io.Copy(w, r)
			w.Close()
		}
		c.writerLocker.Unlock()
		fallthrough
	case parser.PONG:
		c.pingChan <- true
		if c.getState() == stateUpgrading {
			p := make([]byte, 64)
			_, err := r.Read(p)
			if err == nil && strings.Contains(string(p), "probe") {
				c.writerLocker.Lock()
				w, _ := c.getUpgrade().NextWriter(message.MessageText, parser.UPGRADE)
				if w != nil {
					io.Copy(w, r)
					w.Close()
				}
				c.writerLocker.Unlock()

				c.upgraded()
			}
		}
	case parser.MESSAGE:
		closeChan := make(chan struct{})
		c.readerChan <- newConnReader(r, closeChan)
		<-closeChan
		close(closeChan)
		r.Close()
	case parser.UPGRADE:
		c.upgraded()
	case parser.NOOP:
	}
}

func (c *clientConn) OnClose(server transport.Client) {
	if t := c.getUpgrade(); server == t {
		c.setUpgrading("", nil)
		t.Close()
		return
	}
	t := c.getCurrent()
	if server != t {
		return
	}
	t.Close()
	if t := c.getUpgrade(); t != nil {
		t.Close()
		c.setUpgrading("", nil)
	}
	c.setState(stateClosed)
	close(c.readerChan)
	close(c.pingChan)
}

func (c *clientConn) onOpen() error {

	var err error
	if (len(c.options.Transport) == 2 &&
		((c.options.Transport[0] == "polling" && c.options.Transport[1] == "websocket") ||
			(c.options.Transport[1] == "polling" && c.options.Transport[0] == "websocket"))) ||

		(len(c.options.Transport) == 1 && c.options.Transport[0] == "polling") {

		c.request, err = http.NewRequest("GET", c.url.String(), nil)
		if err != nil {
			return err
		}

		creater, exists := creators["polling"]
		if !exists {
			return InvalidError
		}

		q := c.request.URL.Query()
		q.Set("transport", "polling")
		c.request.URL.RawQuery = q.Encode()
		if c.options.Header != nil {
			c.request.Header = c.options.Header
		}

		transport, err := creater.Client(c.request)
		if err != nil {
			return err
		}
		c.setCurrent("polling", transport)

		pack, err := c.getCurrent().NextReader()
		if err != nil {
			return err
		}

		p := make([]byte, 4096)
		l, err := pack.Read(p)
		if err != nil {
			return err
		}

		type connectionInfo struct {
			Sid          string        `json:"sid"`
			Upgrades     []string      `json:"upgrades"`
			PingInterval time.Duration `json:"pingInterval"`
			PingTimeout  time.Duration `json:"pingTimeout"`
		}

		var msg connectionInfo
		err = json.Unmarshal(p[:l], &msg)
		if err != nil {
			return err
		}
		msg.PingInterval *= 1000 * 1000
		msg.PingTimeout *= 1000 * 1000

		c.pingInterval = msg.PingInterval
		c.pingTimeout = msg.PingTimeout
		c.id = msg.Sid

		if len(c.options.Transport) == 1 && c.options.Transport[0] == "polling" {
			//over
		} else if len(c.options.Transport) == 2 &&
			(c.options.Transport[0] == "websocket" ||
				c.options.Transport[1] == "websocket") {
			//upgrade
			creater, exists = creators["websocket"]
			if !exists {
				return InvalidError
			}

			if c.request.URL.Scheme == "https" {
				c.request.URL.Scheme = "wss"
			} else {
				c.request.URL.Scheme = "ws"
			}
			q.Set("sid", c.id)
			q.Set("transport", "websocket")
			c.request.URL.RawQuery = q.Encode()

			transport, err = creater.Client(c.request)
			if err != nil {
				return err
			}
			c.setUpgrading("websocket", transport)

			w, err := c.getUpgrade().NextWriter(message.MessageText, parser.PING)
			if err != nil {
				return err
			}
			w.Write([]byte("probe"))
			w.Close()
		} else {
			return InvalidError
		}
		return nil
	} else if len(c.options.Transport) == 1 && c.options.Transport[0] == "websocket" {
		c.request, err = http.NewRequest("GET", c.url.String(), nil)
		if err != nil {
			return err
		}

		if c.request.URL.Scheme == "https" {
			c.request.URL.Scheme = "wss"
		} else {
			c.request.URL.Scheme = "ws"
		}

		creater, exists := creators["websocket"]
		if !exists {
			return InvalidError
		}

		q := c.request.URL.Query()
		q.Set("transport", "websocket")
		c.request.URL.RawQuery = q.Encode()
		if c.options.Header != nil {
			c.request.Header = c.options.Header
		}

		transport, err := creater.Client(c.request)
		if err != nil {
			return err
		}
		c.setUpgrading("websocket", transport)

		pack, err := c.getUpgrade().NextReader()
		if err != nil {
			return err
		}

		p := make([]byte, 4096)
		l, err := pack.Read(p)
		if err != nil {
			return err
		}

		type connectionInfo struct {
			Sid          string        `json:"sid"`
			Upgrades     []string      `json:"upgrades"`
			PingInterval time.Duration `json:"pingInterval"`
			PingTimeout  time.Duration `json:"pingTimeout"`
		}

		var msg connectionInfo
		err = json.Unmarshal(p[:l], &msg)
		if err != nil {
			return err
		}
		msg.PingInterval *= 1000 * 1000
		msg.PingTimeout *= 1000 * 1000

		c.pingInterval = msg.PingInterval
		c.pingTimeout = msg.PingTimeout
		c.id = msg.Sid

		//upgrade

		q.Set("sid", c.id)
		q.Set("transport", "websocket")
		c.request.URL.RawQuery = q.Encode()

		//transport, err = creater.Client(c.request)
		if err != nil {
			return err
		}
		c.setCurrent("websocket", transport)
		c.setState(stateNormal)

		// w, err := c.getCurrent().NextWriter(message.MessageText, parser.PING)
		// if err != nil {
		// 	return err
		// }
		// //w.Write([]byte("probe"))
		// w.Close()

		return nil
	}
	return InvalidError
}

func (c *clientConn) getCurrent() transport.Client {
	c.transportLocker.RLock()
	defer c.transportLocker.RUnlock()

	return c.current
}

func (c *clientConn) getUpgrade() transport.Client {
	c.transportLocker.RLock()
	defer c.transportLocker.RUnlock()

	return c.upgrading
}

func (c *clientConn) setCurrent(name string, s transport.Client) {
	c.transportLocker.Lock()
	defer c.transportLocker.Unlock()

	c.currentName = name
	c.current = s
}

func (c *clientConn) setUpgrading(name string, s transport.Client) {
	c.transportLocker.Lock()
	defer c.transportLocker.Unlock()

	c.upgradingName = name
	c.upgrading = s
	c.setState(stateUpgrading)
}

func (c *clientConn) upgraded() {
	c.transportLocker.Lock()

	current := c.current
	c.current = c.upgrading
	c.currentName = c.upgradingName
	c.upgrading = nil
	c.upgradingName = ""

	c.transportLocker.Unlock()

	current.Close()
	c.setState(stateNormal)
}

func (c *clientConn) getState() state {
	c.stateLocker.RLock()
	defer c.stateLocker.RUnlock()
	return c.state
}

func (c *clientConn) setState(state state) {
	c.stateLocker.Lock()
	defer c.stateLocker.Unlock()
	c.state = state
}

func (c *clientConn) pingLoop() {
	defer c.Close()
	// set interval for ping
	ticker := time.NewTicker(c.pingInterval)
	for {
		var ierr error
		go func() {
			// send ping
			c.writerLocker.Lock()
			defer c.writerLocker.Unlock()
			w, err := c.getCurrent().NextWriter(message.MessageText, parser.PING)
			if err != nil {
				ierr = err
				return
			}
			defer w.Close()
		}()
		if ierr != nil {
			log.Errorf("pingLoop failed, %v", ierr)
			return
		}
		// receive pong msg, or trigger timeout for pong msg
		select {
		case <-c.pingChan:

		case <-time.After(c.pingTimeout):
			return
		}

		//Prevent accidental pong stuck on top select
	for_Ticker:
		for {
			select {
			case <-ticker.C:
				break for_Ticker
			case <-c.pingChan:
				continue
			}
		}
	}
}

func (c *clientConn) readLoop() {
	current := c.getCurrent()
	defer c.OnClose(current)
	for {
		current = c.getCurrent()
		if c.getUpgrade() != nil {
			current = c.getUpgrade()
		}
		pack, err := current.NextReader()
		if err != nil {
			return
		}
		c.OnPacket(pack)
		pack.Close()
	}
}
