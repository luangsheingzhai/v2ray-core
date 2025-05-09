//go:build !confonly
// +build !confonly

package websocket

import (
	"context"
	"crypto/tls"
	"net/http"
	"sync"
	"time"

	"github.com/gorilla/websocket"
	"github.com/pires/go-proxyproto"

	"v2ray.com/core/common"
	"v2ray.com/core/common/net"
	http_proto "v2ray.com/core/common/protocol/http"
	"v2ray.com/core/common/session"
	"v2ray.com/core/transport/internet"
	v2tls "v2ray.com/core/transport/internet/tls"
)

type requestHandler struct {
	path string
	ln   *Listener
}

var forbiddenContent = []byte(`<html>
<head><title>403 Forbidden</title></head>
<body>
<center><h1>403 Forbidden</h1></center>
<hr><center>nginx</center>
</body>
</html>`)

var notFoundContent = []byte(`<html>
<head><title>404 Not Found</title></head>
<body>
<center><h1>404 Not Found</h1></center>
<hr><center>nginx</center>
</body>
</html>`)

func forbiddenHandler(writer http.ResponseWriter) {
	// 先设置响应头
	writer.Header().Set("Server", "nginx")
	writer.Header().Set("Content-Type", "text/html")
	// 然后设置状态码
	writer.WriteHeader(http.StatusForbidden)
	// 最后写入响应体
	_, err := writer.Write(forbiddenContent)
	if err != nil {
		newError("failed to write forbidden response:").Base(err).WriteToLog()
		return
	}
}

func notFoundHandler(writer http.ResponseWriter) {
	// 先设置响应头
	writer.Header().Set("Server", "nginx")
	writer.Header().Set("Content-Type", "text/html")
	// 然后设置状态码
	writer.WriteHeader(http.StatusNotFound)
	// 最后写入响应体
	_, err := writer.Write(notFoundContent)
	if err != nil {
		newError("failed to write not found response:").Base(err).WriteToLog()
		return
	}
}

var upgrader = &websocket.Upgrader{
	ReadBufferSize:   4 * 1024,
	WriteBufferSize:  4 * 1024,
	HandshakeTimeout: time.Second * 4,
	CheckOrigin: func(r *http.Request) bool {
		return true
	},
	Error: func(writer http.ResponseWriter, r *http.Request, status int, reason error) {
		forbiddenHandler(writer)
	},
}

func (h *requestHandler) ServeHTTP(writer http.ResponseWriter, request *http.Request) {
	if request.URL.Path != h.path {
		notFoundHandler(writer)
		return
	}
	conn, err := upgrader.Upgrade(writer, request, nil)
	if err != nil {
		newError("failed to convert to WebSocket connection").Base(err).WriteToLog()
		return
	}

	forwardedAddrs := http_proto.ParseXForwardedFor(request.Header)
	remoteAddr := conn.RemoteAddr()
	if len(forwardedAddrs) > 0 && forwardedAddrs[0].Family().IsIP() {
		remoteAddr.(*net.TCPAddr).IP = forwardedAddrs[0].IP()
	}

	h.ln.addConn(newConnection(conn, remoteAddr))
}

type Listener struct {
	sync.Mutex
	server   http.Server
	listener net.Listener
	config   *Config
	addConn  internet.ConnHandler
}

func ListenWS(ctx context.Context, address net.Address, port net.Port, streamSettings *internet.MemoryStreamConfig, addConn internet.ConnHandler) (internet.Listener, error) {
	listener, err := internet.ListenSystem(ctx, &net.TCPAddr{
		IP:   address.IP(),
		Port: int(port),
	}, streamSettings.SocketSettings)
	if err != nil {
		return nil, newError("failed to listen TCP(for WS) on", address, ":", port).Base(err)
	}
	newError("listening TCP(for WS) on ", address, ":", port).WriteToLog(session.ExportIDToError(ctx))

	wsSettings := streamSettings.ProtocolSettings.(*Config)

	if wsSettings.AcceptProxyProtocol {
		policyFunc := func(upstream net.Addr) (proxyproto.Policy, error) { return proxyproto.REQUIRE, nil }
		listener = &proxyproto.Listener{Listener: listener, Policy: policyFunc}
		newError("accepting PROXY protocol").AtWarning().WriteToLog(session.ExportIDToError(ctx))
	}

	if config := v2tls.ConfigFromStreamSettings(streamSettings); config != nil {
		if tlsConfig := config.GetTLSConfig(); tlsConfig != nil {
			listener = tls.NewListener(listener, tlsConfig)
		}
	}

	l := &Listener{
		config:   wsSettings,
		addConn:  addConn,
		listener: listener,
	}

	l.server = http.Server{
		Handler: &requestHandler{
			path: wsSettings.GetNormalizedPath(),
			ln:   l,
		},
		ReadHeaderTimeout: time.Second * 4,
		MaxHeaderBytes:    2048,
	}

	go func() {
		if err := l.server.Serve(l.listener); err != nil {
			newError("failed to serve http for WebSocket").Base(err).AtWarning().WriteToLog(session.ExportIDToError(ctx))
		}
	}()

	return l, err
}

// Addr implements net.Listener.Addr().
func (ln *Listener) Addr() net.Addr {
	return ln.listener.Addr()
}

// Close implements net.Listener.Close().
func (ln *Listener) Close() error {
	return ln.listener.Close()
}

func init() {
	common.Must(internet.RegisterTransportListener(protocolName, ListenWS))
}
