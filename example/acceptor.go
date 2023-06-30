package main

import (
    "fmt"
	"sync"
	"syscall"

	"github.com/shaovie/goev"
)

var (
	buffPool *sync.Pool
)

const httpResp = "HTTP/1.1 200 OK\r\nConnection: close\r\nContent-Length: 5\r\n\r\nhello"

type Http struct {
	goev.NullEvHandler
    m [4096]byte // test memory leak
}

func (h *Http) OnOpen(fd *goev.Fd) bool {
	return true
}
func (h *Http) OnRead(fd *goev.Fd) bool {
	buf := buffPool.Get().([]byte) // just read
    defer buffPool.Put(buf)

    readN := 0
	for {
        if readN >= cap(buf) { // alloc new buff to read
            break
        }
        n, err := fd.Read(buf[readN:])
		if err != nil {
			if err == syscall.EAGAIN { // epoll ET mode
				break
			} else if err == syscall.EINTR { // MUST
                continue
            }
            fmt.Println("read: ", err.Error())
			return false
		}
		if n > 0 { // n > 0
            readN += n
        } else { // n == 0 connection closed,  will not < 0
            if readN == 0 {
                fmt.Println("peer closed. ", n)
            }
			return false
        }
	}
	fd.Write([]byte(httpResp)) // Connection: close
	return false // will goto OnClose
}
func (h *Http) OnClose(fd *goev.Fd) {
	fd.Close()
}

type Https struct {
	Http
}

func main() {
    fmt.Println("hello boy")
	buffPool = &sync.Pool{
		New: func() any {
			return make([]byte, 4096)
		},
	}
	r, err := goev.NewReactor(
		goev.EvPollSize(1024),
		goev.EvPollThreadNum(0), // auto calc
	)
	if err != nil {
		panic(err.Error())
	}
	if err = r.Open(); err != nil {
		panic(err.Error())
	}
	//= http
	httpAcceptor, err := goev.NewAcceptor(
		goev.ListenBacklog(256),
		goev.RecvBuffSize(8*1024), // 短链接, 不需要很大的缓冲区
	)
	if err != nil {
		panic(err.Error())
	}
	httpAcceptor.Open(r, func() goev.EvHandler {
		return new(Http)
	},
		":2023",
		goev.EV_IN,
	)

	//= https
	httpsAcceptor, err := goev.NewAcceptor(
		goev.ListenBacklog(256),
		goev.RecvBuffSize(8*1024), // 短链接, 不需要很大的缓冲区
	)
	if err != nil {
		panic(err.Error())
	}
	httpsAcceptor.Open(r, func() goev.EvHandler {
		return new(Https)
	},
		":2024",
		goev.EV_IN,
	)

	if err = r.Run(); err != nil {
		panic(err.Error())
	}
}