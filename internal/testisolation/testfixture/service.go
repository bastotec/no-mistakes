package testfixture

import (
	"net"
	"net/http"
	"time"
)

// Start is used only by the confinement helper, never by the production worker.
func Start() (func(), error) {
	handler, closeDB, err := DatabaseHandler("/tmp/fixture.sqlite")
	if err != nil {
		return nil, err
	}
	listener, err := net.Listen("tcp4", "127.0.0.1:17681")
	if err != nil {
		closeDB()
		return nil, err
	}
	server := &http.Server{Handler: handler, ReadHeaderTimeout: 5 * time.Second}
	go server.Serve(listener)
	return func() { server.Close(); closeDB() }, nil
}
