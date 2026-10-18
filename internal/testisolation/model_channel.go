package testisolation

import (
	"net"
	"sync"
)

// channelListener exposes one preconnected stream, not a filesystem/abstract
// Unix socket another descendant could repoint at a host service.
type channelListener struct {
	connection net.Conn
	mu         sync.Mutex
	accepted   bool
	closed     chan struct{}
	once       sync.Once
}

func newChannelListener(c net.Conn) *channelListener {
	return &channelListener{connection: c, closed: make(chan struct{})}
}
func (l *channelListener) Accept() (net.Conn, error) {
	l.mu.Lock()
	if !l.accepted {
		l.accepted = true
		l.mu.Unlock()
		return l.connection, nil
	}
	l.mu.Unlock()
	<-l.closed
	return nil, net.ErrClosed
}
func (l *channelListener) Close() error {
	l.once.Do(func() { l.connection.Close(); close(l.closed) })
	return nil
}
func (l *channelListener) Addr() net.Addr { return l.connection.LocalAddr() }
