package proxy

import (
	"net"
	"time"
)

const ShutdownTimeout = 5 * time.Second

func Listen(addr string) (net.Listener, error) {
	return net.Listen("tcp", addr)
}
