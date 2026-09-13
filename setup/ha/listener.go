package ha

import (
	"fmt"
	"net"
	"net/http"
	"time"

	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/sirupsen/logrus"

	"codefloe.com/pat-s/zendrite/internal/httputil"
	"codefloe.com/pat-s/zendrite/setup/config"
)

// statusReadHeaderTimeout bounds probe connections, which are never long-lived.
const statusReadHeaderTimeout = 5 * time.Second

// listenStatus binds the candidate status listener.
//
// It is separate from the Matrix listeners on purpose. Those are bound only
// during activation, so a standby would otherwise answer nothing: its liveness
// probe would fail and Kubernetes would restart a healthy candidate, and its
// metrics would be unscrapable until it happened to be promoted.
func listenStatus(cfg config.HA, status *Status, metrics *config.Metrics) (net.Listener, func(), error) {
	mux := http.NewServeMux()
	mux.Handle("/_zendrite/monitor/", status.Handler())
	if metrics != nil && metrics.Enabled {
		mux.Handle("GET /metrics", httputil.WrapHandlerInBasicAuth(promhttp.Handler(), metrics.BasicAuth))
	}
	listener, err := net.Listen("tcp", cfg.StatusAddress)
	if err != nil {
		return nil, nil, fmt.Errorf("bind HA status listener on %q: %w", cfg.StatusAddress, err)
	}
	server := &http.Server{Handler: mux, ReadHeaderTimeout: statusReadHeaderTimeout}
	return listener, func() {
		if err := server.Serve(listener); err != nil && err != http.ErrServerClosed {
			logrus.WithError(err).Error("HA status listener failed")
		}
	}, nil
}
