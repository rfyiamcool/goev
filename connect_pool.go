package goev

import (
	"container/list"
	"errors"
	"runtime"
	"sync"
	"sync/atomic"
	"time"

	"github.com/shaovie/goev/netfd"
)

// ConnectPoolHandler is the interface that wraps the basic Conn handle method
type ConnectPoolHandler interface {
	EvHandler

	setPool(cp *ConnectPool)

	GetPool() *ConnectPool

	Closed()
}

// ConnectPoolItem is the base object
type ConnectPoolItem struct {
	Event

	cp *ConnectPool
}

func (cph *ConnectPoolItem) setPool(cp *ConnectPool) {
	cph.cp = cp
}

// GetPool can retrieve the current conn object bound to which ConnectPool
func (cph *ConnectPoolItem) GetPool() *ConnectPool {
	return cph.cp
}

// Closed when a conn is detected as closed, it needs to notify the ConnectPool to perform resource recycling.
func (cph *ConnectPoolItem) Closed() {
	cph.cp.closed()
}

// ConnectPool provides a reusable connection pool that can dynamically scale and manage network connections
type ConnectPool struct {
	minIdleNum     int
	addNumOnceTime int
	maxLiveNum     int
	addr           string
	connector      *Connector

	ticker                    *time.Ticker
	conns                     *list.List
	connsMtx                  sync.Mutex
	toNewNum                  atomic.Int32
	liveNum                   atomic.Int32
	newConnectPoolHandlerFunc func() ConnectPoolHandler

	emptySig    chan struct{}
	newConnChan chan newConnInPool
}
type newConnInPool struct {
	fd int
	ch ConnectPoolHandler
}

// NewConnectPool return an instance
//
// The addr format 192.168.0.1:8080
func NewConnectPool(c *Connector, addr string, minIdleNum, addNumOnceTime, maxLiveNum int,
	newConnectPoolHandlerFunc func() ConnectPoolHandler) (*ConnectPool, error) {

	if minIdleNum < 1 || minIdleNum >= maxLiveNum || maxLiveNum < addNumOnceTime {
		panic("NewConnectPool min/add/max  invalid")
	}
	r := c.GetReactor()
	if r == nil {
		return nil, errors.New("connector invalid")
	}
	cp := &ConnectPool{
		minIdleNum:                minIdleNum,
		addNumOnceTime:            addNumOnceTime,
		maxLiveNum:                maxLiveNum,
		addr:                      addr,
		connector:                 c,
		conns:                     list.New(),
		newConnectPoolHandlerFunc: newConnectPoolHandlerFunc,
		ticker:                    time.NewTicker(time.Millisecond * 200),
		newConnChan:               make(chan newConnInPool, runtime.NumCPU()*2),
		emptySig:                  make(chan struct{}),
	}

	go cp.keepNumTiming()
	go cp.handleNewConn()
	return cp, nil
}

// Acquire returns a usable connection handler, and if none is available, it returns nil
func (cp *ConnectPool) Acquire() ConnectPoolHandler {
	cp.connsMtx.Lock()
	item := cp.conns.Front()
	if item == nil {
		cp.connsMtx.Unlock()
		cp.emptySig <- struct{}{}
		return nil
	}
	cp.conns.Remove(item)
	cp.connsMtx.Unlock()
	return item.Value.(ConnectPoolHandler)
}

// Release accepts a reusable connection
func (cp *ConnectPool) Release(ch ConnectPoolHandler) {
	if ch.GetPool() != cp {
		panic("ConnectPool.Release ch doesn't belong to this pool")
	}
	if ch == nil {
		panic("ConnectPool.Release ch is nil")
	}
	cp.connsMtx.Lock()
	cp.conns.PushBack(ch)
	cp.connsMtx.Unlock()
}

// IdleNum returns the number of idle connections
func (cp *ConnectPool) IdleNum() int {
	cp.connsMtx.Lock()
	defer cp.connsMtx.Unlock()
	return cp.conns.Len()
}

// LiveNum returns the number of active connections
func (cp *ConnectPool) LiveNum() int {
	return int(cp.liveNum.Load())
}

func (cp *ConnectPool) keepNumTiming() {
	for {
		select {
		case <-cp.emptySig:
			cp.keepNum()
		case <-cp.ticker.C:
			cp.keepNum()
		}
	}
}
func (cp *ConnectPool) keepNum() {
	// 1. keep min size
	idleNum := cp.IdleNum()
	toNewNum := 0
	if idleNum < cp.minIdleNum {
		toNewNum = cp.addNumOnceTime
		liveNum := cp.LiveNum()
		if liveNum == 0 {
			toNewNum = cp.minIdleNum
		} else if toNewNum+liveNum > cp.maxLiveNum {
			toNewNum = cp.maxLiveNum - liveNum
		}
	}
	if toNewNum < 1 {
		return
	}

	if !cp.toNewNum.CompareAndSwap(0, int32(toNewNum)) {
		return
	}
	for i := 0; i < toNewNum; i++ {
		if err := cp.connector.Connect(cp.addr, &connectPoolConn{cp: cp}, 1000); err != nil {
			cp.toNewNum.Add(-1)
		}
	}
}
func (cp *ConnectPool) handleNewConn() {
	for {
		select {
		case conn := <-cp.newConnChan:
			cp.onNewConn(conn.fd, conn.ch)
		}
	}
}
func (cp *ConnectPool) onNewConn(fd int, ch ConnectPoolHandler) {
	if ch.OnOpen(fd, time.Now().UnixMilli()) == false {
		return
	}
	cp.liveNum.Add(1)
	cp.Release(ch)
}
func (cp *ConnectPool) closed() {
	cp.liveNum.Add(-1)
}

type connectPoolConn struct {
	Event

	cp *ConnectPool
}

func (cpc *connectPoolConn) OnOpen(fd int, now int64) bool {
	cpc.cp.toNewNum.Add(-1)

	netfd.SetKeepAlive(fd, 60, 40, 3)

	connHandler := cpc.cp.newConnectPoolHandlerFunc()
	connHandler.setReactor(cpc.GetReactor())
	connHandler.setPool(cpc.cp)
	cpc.cp.newConnChan <- newConnInPool{fd: fd, ch: connHandler}

	return false
}
func (cpc *connectPoolConn) OnConnectFail(err error) {
	cpc.cp.toNewNum.Add(-1)
}
func (cpc *connectPoolConn) OnClose(fd int) {
}
