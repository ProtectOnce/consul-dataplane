package consuldp

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"sync"
	"time"

	"github.com/armon/go-metrics"
	"github.com/armon/go-metrics/prometheus"

	"github.com/hashicorp/consul-dataplane/internal/bootstrap"
	"github.com/hashicorp/go-hclog"
	prom "github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

type Stats int

const (
	// This is the listener that the admin endpoint will be available on.
	// TODO: Make this configurable
	envoyMetricsUrl = "http://127.0.0.1:19000/stats/prometheus"

	// The consul dataplane specific metrics will be exposed on this port on the loopback
	cdpMetricsBindPort = "20101"
	cdpMetricsBindAddr = "127.0.0.1:" + cdpMetricsBindPort

	// mergedMetricsBackendBindPort is the port which will serve the merged
	// metrics and the port that envoy is exposing a scrape url for prometheus
	mergedMetricsBackendBindPort = "20100"
	metricsBackendBindAddr       = "127.0.0.1:" + mergedMetricsBackendBindPort

	// Distinguishing values for the backend stats
	Prometheus Stats = iota
	Dogstatsd
	Statsd
)

var (
	cdpMetricsUrl = fmt.Sprintf("http://%s", cdpMetricsBindAddr)
)

// metricsConfig handles all configuration related to merging
// the metrics and presenting them on promServer
type metricsConfig struct {
	logger hclog.Logger
	cfg    *TelemetryConfig

	// merged metrics config
	promServer *http.Server // the server that will will serve all the merged metrics
	client     httpGetter   // the client that will scrape the urls
	urls       []string     // the urls that will be scraped

	// consuldp metrics
	promScrapeServer *http.Server // cdp metrics prometheus scrape server

	// lifecycle control
	errorExitCh chan struct{}
	running     bool
	mu          sync.Mutex
	cancelFn    context.CancelFunc
}

func NewMetricsConfig(cfg *TelemetryConfig) *metricsConfig {
	return &metricsConfig{
		mu:          sync.Mutex{},
		cfg:         cfg,
		errorExitCh: make(chan struct{}),

		client: &http.Client{
			Timeout: 10 * time.Second,
		},
	}
}

func (m *metricsConfig) startMetrics(ctx context.Context, bcfg *bootstrap.BootstrapConfig) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.running {
		return nil
	}

	if m.cfg.UseCentralConfig {

		m.logger = hclog.FromContext(ctx).Named("metrics")
		ctx, cancel := context.WithCancel(ctx)
		m.cancelFn = cancel
		m.running = true

		switch {
		case bcfg.PrometheusBindAddr != "":
			// 1. start consul dataplane metric sinks of type Prometheus
			err := m.configureCDPMetricSinks(ctx, Prometheus)
			if err != nil {
				return fmt.Errorf("failure enabling consul dataplane metrics for prometheus: %w", err)
			}
			// 2. Setup prometheus handler for the merged metrics endpoint that prometheus
			// will actually scrape
			mux := http.NewServeMux()
			mux.HandleFunc("/stats/prometheus", m.mergedMetricsHandler)
			m.urls = []string{cdpMetricsUrl, envoyMetricsUrl}
			if m.cfg != nil && m.cfg.Prometheus.ServiceMetricsURL != "" {
				m.urls = append(m.urls, m.cfg.Prometheus.ServiceMetricsURL)
			}
			m.promServer = &http.Server{
				Addr:    metricsBackendBindAddr,
				Handler: mux,
			}

			// Start prometheus metrics sink
			go m.startPrometheusMetricsSink(ctx)

		case bcfg.StatsdURL != "":
			// TODO: send merged metrics
		case bcfg.DogstatsdURL != "":
			// TODO: send merged metrics
		}
	}

	go func() {
		<-ctx.Done()
		m.stopMetricsServers()
	}()
	return nil
}

func (m *metricsConfig) Cancel() {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.cancelFn != nil {
		m.cancelFn()
	}
}

func (m *metricsConfig) startPrometheusMetricsSink(ctx context.Context) {

	m.logger.Info("starting merged metrics server", "address", m.promServer.Addr)
	err := m.promServer.ListenAndServe()
	if err != nil && err != http.ErrServerClosed {
		m.logger.Error("failed to serve metrics requests", "error", err)
		close(m.errorExitCh)
	}
}

func (m *metricsConfig) stopMetricsServers() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.running = false

	if m.promServer != nil {
		m.logger.Info("stopping the merged  server")
		err := m.promServer.Close()
		if err != nil {
			m.logger.Warn("error while closing metrics server", "error", err)
			close(m.errorExitCh)
		}
	}
	if m.promScrapeServer != nil {
		m.logger.Info("stopping consul dp promtheus server")
		err := m.promScrapeServer.Close()
		if err != nil {
			m.logger.Warn("error while closing metrics server", "error", err)
			close(m.errorExitCh)
		}
	}
}

func (m *metricsConfig) metricsServerExited() <-chan struct{} {
	return m.errorExitCh
}

// mergedMetricsHandler responds with merged metrics from multiple sources:
// Consul Dataplane, Envoy and (optionally) the service/application. The Envoy
// and service metrics are scraped synchronously during the handling of this
// request.
func (m *metricsConfig) mergedMetricsHandler(rw http.ResponseWriter, _ *http.Request) {
	for _, url := range m.urls {
		m.logger.Debug("scraping url for merging", "url", url)
		if err := m.scrapeMetrics(rw, url); err != nil {
			m.scrapeError(rw, url, err)
			return
		}
	}
}

// scrapeMetrics fetches metrics from the given url and copies them to the response.
func (m *metricsConfig) scrapeMetrics(rw http.ResponseWriter, url string) error {
	resp, err := m.client.Get(url)
	if err != nil {
		return err
	}
	defer func() {
		err := resp.Body.Close()
		if err != nil {
			m.logger.Warn("failed to close metrics request", "error", err)
		}
	}()

	if non2xxCode(resp.StatusCode) {
		return fmt.Errorf("status code %d", resp.StatusCode)
	}

	// Prometheus metrics are joined by newlines, so when merging metrics
	// metrics we simply write all lines from each source to the response.
	_, err = io.Copy(rw, resp.Body)
	return err
}

// scrapeError logs an error and responds to the http request with an error.
func (m *metricsConfig) scrapeError(rw http.ResponseWriter, url string, err error) {
	m.logger.Error("failed to scrape metrics", "url", url, "error", err)
	msg := fmt.Sprintf("failed to scrape metrics at url %q", url)
	http.Error(rw, msg, http.StatusInternalServerError)
}

// non2xxCode returns true if code is not in the range of 200-299 inclusive.
func non2xxCode(code int) bool {
	return code < 200 || code >= 300
}

func (m *metricsConfig) getPromDefaults() (*prom.Registry, *prometheus.PrometheusOpts, error) {
	r := prom.NewRegistry()
	reg := prom.WrapRegistererWithPrefix("consul_dataplane_", r)
	err := reg.Register(collectors.NewGoCollector())
	if err != nil {
		return nil, nil, err
	}
	opts := &prometheus.PrometheusOpts{
		Registerer: reg,
		// GaugeDefinitions: ,
		// CounterDefinitions: ,
		// SummaryDefinitions: ,
	}
	return r, opts, nil
}

// configureCDPMetricSinks
func (m *metricsConfig) configureCDPMetricSinks(ctx context.Context, s Stats) error {

	switch s {
	case Prometheus:
		r, opts, err := m.getPromDefaults()
		if err != nil {
			return err
		}
		sink, err := prometheus.NewPrometheusSinkFrom(*opts)
		if err != nil {
			return err
		}
		conf := metrics.DefaultConfig("consul_dataplane")
		conf.EnableHostname = false
		_, err = metrics.NewGlobal(conf, sink)
		if err != nil {
			return err
		}

		go m.runCDPPrometheusServer(r)

	case Dogstatsd:
		// TODO
		// datadog.NewDogStatsdSink()
	case Statsd:
		// TODO
		// metrics.NewStatsdSink()
	}
	return nil

}

func (m *metricsConfig) runCDPPrometheusServer(gather prom.Gatherer) {
	m.promScrapeServer = &http.Server{
		Addr: cdpMetricsBindAddr,
		Handler: promhttp.HandlerFor(gather, promhttp.HandlerOpts{
			ErrorHandling: promhttp.ContinueOnError,
		}),
	}
	err := m.promScrapeServer.ListenAndServe()
	if err != nil && err != http.ErrServerClosed {
		m.logger.Error("failed to serve metrics requests", "error", err)
		close(m.errorExitCh)
	}
}
