package main

import (
	"flag"
	"fmt"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/prometheus/common/version"
)

type MonitoringService int

const (
	Prometheus MonitoringService = 1
	InfluxDB   MonitoringService = 2
)

type promHTTPLogger struct{}

func (l promHTTPLogger) Println(v ...interface{}) {
	log.Println(v...)
}

func main() {
	log.Printf("Tinode metrics exporter.")

	var (
		serveFor     = flag.String("serve_for", "influxdb", "Monitoring service to gather metrics for. Available: influxdb, prometheus.")
		tinodeAddr   = flag.String("tinode_addr", "http://localhost:6060/stats/expvar", "Address of the Tinode instance to scrape.")
		listenAt     = flag.String("listen_at", ":6222", "Host name and port to listen for incoming requests on.")
		metricList   = flag.String("metric_list", "Version,LiveTopics,TotalTopics,LiveSessions,ClusterLeader,TotalClusterNodes,LiveClusterNodes,memstats.Alloc", "Comma-separated list of metrics to scrape and export.")
		instance     = flag.String("instance", "exporter", "Exporter instance name.")

		// Prometheus-specific arguments.
		promNamespace    = flag.String("prom_namespace", "tinode", "Prometheus namespace for metrics '<namespace>_...'")
		promMetricsPath  = flag.String("prom_metrics_path", "/metrics", "Path under which to expose metrics for Prometheus scrapes.")
		promTimeout      = flag.Int("prom_timeout", 15, "Tinode connection timeout in seconds in response to Prometheus scrapes.")

		// InfluxDB-specific arguments.
		influxPushAddr     = flag.String("influx_push_addr", "http://localhost:9999/write", "Address of InfluxDB target server where the data gets sent.")
		influxDBVersion    = flag.String("influx_db_version", "1.7", "Version of InfluxDB (only 1.7 and 2.0 are supported).")
		influxOrganization = flag.String("influx_organization", "test", "InfluxDB organization to push metrics as.")
		influxBucket       = flag.String("influx_bucket", "test", "InfluxDB storage bucket to store data in (used only in InfluxDB 2.0).")
		influxAuthToken    = flag.String("influx_auth_token", "", "InfluxDB authentication token.")
		influxPushInterval = flag.Int("influx_push_interval", 30, "InfluxDB push interval in seconds.")
	)
	flag.Parse()

	var service MonitoringService
	if *serveFor == "prometheus" {
		service = Prometheus
	} else if *serveFor == "influxdb" {
		service = InfluxDB
	} else {
		log.Fatal("Invalid monitoring service:" + *serveFor + "; must be either \"prometheus\" or \"influxdb\"")
	}
	// Validate flags.
	switch service {
	case Prometheus:
		if *promMetricsPath == "/" {
			log.Fatal("Serving metrics from / is not supported")
		}
	case InfluxDB:
		if *influxOrganization == "" {
			log.Fatal("Must specify --influx_organization")
		}
		if *influxAuthToken == "" {
			log.Fatal("Must specify --influx_auth_token")
		}
		if *influxBucket == "" {
			log.Fatal("Must specify --influx_bucket")
		}
		if *influxDBVersion != "1.7" && *influxDBVersion != "2.0" {
			log.Fatal("Please, set --influx_db_version to either 1.7 or 2.0")
		}
	}

	// Index page at web root.
	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		var servingPath string
		switch service {
		case Prometheus:
			servingPath = "<p>Prometheus exporter path: <a href='" + *promMetricsPath + "'>Metrics</a></p>"
		case InfluxDB:
			servingPath = "<p>InfluxDB push path: <a href='/push'>Push</a></p>"
		}

		w.Write([]byte(`<html><head><title>Tinode Exporter</title></head><body>
<h1>Tinode Exporter</h1>
<p>Server type` + *serveFor + `</p>` + servingPath +
`<h2>Build</h2>
<pre>` + version.Info() + ` ` + version.BuildContext() + `</pre>
</body></html>`))
	})

	metrics := strings.Split(*metricList, ",")
	scraper := Scraper{address: *tinodeAddr, metrics: metrics}
	var serverTypeString string
	// Create exporters.
	switch service {
	case Prometheus:
		serverTypeString = *serveFor
		promExporter := NewPromExporter(*tinodeAddr, *promNamespace, time.Duration(*promTimeout)*time.Second, &scraper)
		registry := prometheus.NewRegistry()
		registry.MustRegister(promExporter)
		http.Handle(*promMetricsPath,
			promhttp.InstrumentMetricHandler(
				registry,
				promhttp.HandlerFor(
					registry,
					promhttp.HandlerOpts{
						ErrorLog: &promHTTPLogger{},
						Timeout:  time.Duration(*promTimeout) * time.Second,
					},
				),
			),
		)
	case InfluxDB:
		serverTypeString = fmt.Sprintf("%s, version %s", *serveFor, *influxDBVersion)
		influxDBExporter := NewInfluxDBExporter(*influxDBVersion, *influxPushAddr, *influxOrganization, *influxBucket, *influxAuthToken, *instance, &scraper)
		if *influxPushInterval > 0 {
			go func() {
				interval := time.Duration(*influxPushInterval) * time.Second
				ch := time.Tick(interval)
				for {
					if _, ok := <-ch; ok {
						if status, err := influxDBExporter.Push(); err != nil {
							log.Printf("InfluxDB push failed (status '%s'), error: %s", status, err.Error())
						}
					} else {
						return
					}
				}
			}()
		} else {
			log.Println("InfluxDB push interval is zero. Will not push data automatically.")
		}
		// Forces a data push.
		http.HandleFunc("/push", func(w http.ResponseWriter, r *http.Request) {
			var msg string
			if http_status, err := influxDBExporter.Push(); err == nil {
				msg = "ok - " + http_status
			} else {
				msg = "fail - " + err.Error()
			}

			w.Write([]byte(`<html><head><title>Tinode Push</title></head><body>
<h1>Tinode Push</h1>
<pre>` + msg + `</pre>
</body></html>`))
		})
	}

	log.Println("Reading Tinode expvar from", *tinodeAddr)
	log.Printf("Exporter running at %s. Server type %s", *listenAt, serverTypeString)
	log.Fatalln(http.ListenAndServe(*listenAt, nil))
}
