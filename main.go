package main

import (
	"context"
	"database/sql"
	"errors"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"net/http"
	"os"
	"path"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/BurntSushi/toml"

	_ "dmdb_exporter/dm"

	"fmt"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/common/log"
	"gopkg.in/alecthomas/kingpin.v2"
	"gopkg.in/ini.v1"
	//Required for debugging
	//_ "net/http/pprof"
)

var (
	// Version will be set at build time.
	Version            = "0.0.0.dev"
	listenAddress      = kingpin.Flag("web.listen-address", "Address to listen on for web interface and telemetry. (env: LISTEN_ADDRESS)").Default(getEnv("LISTEN_ADDRESS", ":9161")).String()
	metricsPath        = kingpin.Flag("web.telemetry-path", "Path under which to expose metrics. (env: TELEMETRY_PATH)").Default(getEnv("TELEMETRY_PATH", "/metrics")).String()
	defaultFileMetrics = kingpin.Flag("default.metrics", "File with default metrics in a TOML file. (env: DEFAULT_METRICS)").Default(getEnv("DEFAULT_METRICS", "default-metrics.toml")).String()
	customMetrics      = kingpin.Flag("custom.metrics", "File that may contain various custom metrics in a TOML file. (env: CUSTOM_METRICS)").Default(getEnv("CUSTOM_METRICS", "")).String()
	queryTimeout       = kingpin.Flag("query.timeout", "Query timeout (in seconds). (env: QUERY_TIMEOUT)").Default(getEnv("QUERY_TIMEOUT", "5")).String()
	maxIdleConns       = kingpin.Flag("database.maxIdleConns", "Number of maximum idle connections in the connection pool. (env: DATABASE_MAXIDLECONNS)").Default(getEnv("DM_MAXIDLECONNS", "0")).Int()
	maxOpenConns       = kingpin.Flag("database.maxOpenConns", "Number of maximum open connections in the connection pool. (env: DATABASE_MAXOPENCONNS)").Default(getEnv("DM_MAXOPENCONNS", "10")).Int()
	config             = kingpin.Flag("config.cnf", "Path to .my.cnf file to read MySQL credentials from.").Default(path.Join(os.Getenv("HOME"), "config.default.cnf")).String()
	dsn                string
	exportConf         *ini.File
)

// Metric name parts.
const (
	namespace = "dmdb"
	exporter  = "exporter"
)

// Metrics object description
type Metric struct {
	Context          string
	Labels           []string
	MetricsDesc      map[string]string
	MetricsType      map[string]string
	FieldToAppend    string
	Request          string
	IgnoreZeroResult bool
}

// Used to load multiple metrics from file
type Metrics struct {
	Metric []Metric
}

// Metrics to scrap. Use external file (default-metrics.toml and custom if provided)
var (
	metricsToScrap    Metrics
	additionalMetrics Metrics
)

// Exporter collects DmService DB metrics. It implements prometheus.Collector.
type Exporter struct {
	dsn             string
	duration, error prometheus.Gauge
	totalScrapes    prometheus.Counter
	scrapeErrors    *prometheus.CounterVec
	up              prometheus.Gauge
	db              *sql.DB
}

// getEnv returns the value of an environment variable, or returns the provided fallback value
func getEnv(key, fallback string) string {
	if value, ok := os.LookupEnv(key); ok {
		return value
	}
	return fallback
}

func connect(dsn string) *sql.DB {
	log.Debugln("Launching connection: ", dsn)

	//db, err := sql.Open("dm", "dm://SYSDBA:SYSDBA@172.20.58.135:5236?autoCommit=true")
	db, err := sql.Open("dm", dsn)

	if err != nil {
		log.Errorln("Error while connecting to", dsn)
		panic(err)
	}
	log.Debugln("set max idle connections to ", *maxIdleConns)
	db.SetMaxIdleConns(*maxIdleConns)
	log.Debugln("set max open connections to ", *maxOpenConns)
	db.SetMaxOpenConns(*maxOpenConns)
	log.Debugln("Successfully connected to: ", dsn)
	return db
}

// NewExporter returns a new DmService DB exporter for the provided DSN.
func NewExporter(dsn string) *Exporter {
	db := connect(dsn)
	return &Exporter{
		dsn: dsn,
		duration: prometheus.NewGauge(prometheus.GaugeOpts{
			Namespace: namespace,
			Subsystem: exporter,
			Name:      "last_scrape_duration_seconds",
			Help:      "Duration of the last scrape of metrics from DM DB.",
		}),
		totalScrapes: prometheus.NewCounter(prometheus.CounterOpts{
			Namespace: namespace,
			Subsystem: exporter,
			Name:      "scrapes_total",
			Help:      "Total number of times DM DB was scraped for metrics.",
		}),
		scrapeErrors: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: namespace,
			Subsystem: exporter,
			Name:      "scrape_errors_total",
			Help:      "Total number of times an error occured scraping a DM database.",
		}, []string{"collector"}),
		error: prometheus.NewGauge(prometheus.GaugeOpts{
			Namespace: namespace,
			Subsystem: exporter,
			Name:      "last_scrape_error",
			Help:      "Whether the last scrape of metrics from DM DB resulted in an error (1 for error, 0 for success).",
		}),
		up: prometheus.NewGauge(prometheus.GaugeOpts{
			Namespace: namespace,
			Name:      "up",
			Help:      "Whether the DM database server is up.",
		}),
		db: db,
	}
}

// Describe describes all the metrics exported by the DM DB exporter.
func (e *Exporter) Describe(ch chan<- *prometheus.Desc) {
	metricCh := make(chan prometheus.Metric)
	doneCh := make(chan struct{})

	go func() {
		for m := range metricCh {
			ch <- m.Desc()
		}
		close(doneCh)
	}()

	e.Collect(metricCh)
	close(metricCh)
	<-doneCh

}

// Collect implements prometheus.Collector.
func (e *Exporter) Collect(ch chan<- prometheus.Metric) {
	e.scrape(ch)
	ch <- e.duration
	ch <- e.totalScrapes
	ch <- e.error
	e.scrapeErrors.Collect(ch)
	ch <- e.up
}

func (e *Exporter) scrape(ch chan<- prometheus.Metric) {
	e.totalScrapes.Inc()
	var err error
	defer func(begun time.Time) {
		e.duration.Set(time.Since(begun).Seconds())
		if err == nil {
			e.error.Set(0)
		} else {
			e.error.Set(1)
		}
	}(time.Now())

	if err = e.db.Ping(); err != nil {
		if strings.Contains(err.Error(), "sql: database is closed") {
			log.Infoln("Reconnecting to DB")
			e.db = connect(e.dsn)
		}
	}
	if err = e.db.Ping(); err != nil {
		log.Errorln("Error pinging dm db:", err)
		//e.db.Close()
		e.up.Set(0)
		return
	} else {
		log.Debugln("Successfully pinged DM database: ")
		e.up.Set(1)
	}

	wg := sync.WaitGroup{}

	for _, metric := range metricsToScrap.Metric {
		wg.Add(1)
		metric := metric //https://golang.org/doc/faq#closures_and_goroutines

		go func() {
			defer wg.Done()

			log.Debugln("About to scrape metric: ")
			log.Debugln("- Metric MetricsDesc: ", metric.MetricsDesc)
			log.Debugln("- Metric Context: ", metric.Context)
			log.Debugln("- Metric MetricsType: ", metric.MetricsType)
			log.Debugln("- Metric Labels: ", metric.Labels)
			log.Debugln("- Metric FieldToAppend: ", metric.FieldToAppend)
			log.Debugln("- Metric IgnoreZeroResult: ", metric.IgnoreZeroResult)
			log.Debugln("- Metric Request: ", metric.Request)

			if len(metric.Request) == 0 {
				log.Errorln("Error scraping for ", metric.MetricsDesc, ". Did you forget to define request in your toml file?")
			}

			if len(metric.MetricsDesc) == 0 {
				log.Errorln("Error scraping for query", metric.Request, ". Did you forget to define metricsdesc  in your toml file?")
			}

			if err = ScrapeMetric(e.db, ch, metric); err != nil {
				log.Errorln("Error scraping for", metric.Context, "_", metric.MetricsDesc, ":", err)
				e.scrapeErrors.WithLabelValues(metric.Context).Inc()
			} else {
				log.Debugln("Successfully scrapped metric: ", metric.Context)
			}
		}()
	}
	wg.Wait()
}

func GetMetricType(metricType string, metricsType map[string]string) prometheus.ValueType {
	var strToPromType = map[string]prometheus.ValueType{
		"gauge":   prometheus.GaugeValue,
		"counter": prometheus.CounterValue,
	}

	strType, ok := metricsType[strings.ToLower(metricType)]
	if !ok {
		return prometheus.GaugeValue
	}
	valueType, ok := strToPromType[strings.ToLower(strType)]
	if !ok {
		panic(errors.New("Error while getting prometheus type " + strings.ToLower(strType)))
	}
	return valueType
}

// interface method to call ScrapeGenericValues using Metric struct values
func ScrapeMetric(db *sql.DB, ch chan<- prometheus.Metric, metricDefinition Metric) error {
	log.Debugln("Calling function ScrapeGenericValues()")
	return ScrapeGenericValues(db, ch, metricDefinition.Context, metricDefinition.Labels,
		metricDefinition.MetricsDesc, metricDefinition.MetricsType,
		metricDefinition.FieldToAppend, metricDefinition.IgnoreZeroResult,
		metricDefinition.Request)
}

// generic method for retrieving metrics.
func ScrapeGenericValues(db *sql.DB, ch chan<- prometheus.Metric, context string, labels []string,
	metricsDesc map[string]string, metricsType map[string]string, fieldToAppend string, ignoreZeroResult bool, request string) error {
	metricsCount := 0
	genericParser := func(row map[string]string) error {
		// Construct labels value
		labelsValues := []string{}
		for _, label := range labels {
			labelsValues = append(labelsValues, row[label])
		}
		// Construct Prometheus values to sent back
		for metric, metricHelp := range metricsDesc {
			value, err := strconv.ParseFloat(strings.TrimSpace(row[metric]), 64)
			// If not a float, skip current metric
			if err != nil {
				log.Errorln("Unable to convert current value to float (metric=" + metric +
					",metricHelp=" + metricHelp + ",value=<" + row[metric] + ">)")
				continue
			}
			log.Debugln("Query result looks like: ", value)
			// If metric do not use a field content in metric's name
			if strings.Compare(fieldToAppend, "") == 0 {
				desc := prometheus.NewDesc(
					prometheus.BuildFQName(namespace, context, metric),
					metricHelp,
					labels, nil,
				)
				ch <- prometheus.MustNewConstMetric(desc, GetMetricType(metric, metricsType), value, labelsValues...)
				// If no labels, use metric name
			} else {
				desc := prometheus.NewDesc(
					prometheus.BuildFQName(namespace, context, cleanName(row[fieldToAppend])),
					metricHelp,
					nil, nil,
				)
				ch <- prometheus.MustNewConstMetric(desc, GetMetricType(metric, metricsType), value)
			}
			metricsCount++
		}
		return nil
	}
	err := GeneratePrometheusMetrics(db, genericParser, request)
	log.Debugln("ScrapeGenericValues() - metricsCount: ", metricsCount)
	if err != nil {
		return err
	}
	if !ignoreZeroResult && metricsCount == 0 {
		return errors.New("No metrics found while parsing")
	}
	return err
}

// inspired by https://kylewbanks.com/blog/query-result-to-map-in-golang
// Parse SQL result and call parsing function to each row
func GeneratePrometheusMetrics(db *sql.DB, parse func(row map[string]string) error, query string) error {

	// Add a timeout
	timeout, err := strconv.Atoi(*queryTimeout)
	if err != nil {
		log.Fatal("error while converting timeout option value: ", err)
		panic(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(timeout)*time.Second)
	defer cancel()
	rows, err := db.QueryContext(ctx, query)

	if ctx.Err() == context.DeadlineExceeded {
		return errors.New("DM query timed out")
	}

	if err != nil {
		return err
	}
	cols, err := rows.Columns()
	defer rows.Close()

	for rows.Next() {
		// Create a slice of interface{}'s to represent each column,
		// and a second slice to contain pointers to each item in the columns slice.
		columns := make([]interface{}, len(cols))
		columnPointers := make([]interface{}, len(cols))
		for i, _ := range columns {
			columnPointers[i] = &columns[i]
		}

		// Scan the result into the column pointers...
		if err := rows.Scan(columnPointers...); err != nil {
			return err
		}

		// Create our map, and retrieve the value for each column from the pointers slice,
		// storing it in the map with the name of the column as the key.
		m := make(map[string]string)
		for i, colName := range cols {
			val := columnPointers[i].(*interface{})
			m[strings.ToLower(colName)] = fmt.Sprintf("%v", *val)
		}
		// Call function to parse row
		if err := parse(m); err != nil {
			return err
		}
	}

	return nil

}

// DB gives us some ugly names back. This function cleans things up for Prometheus.
func cleanName(s string) string {
	s = strings.Replace(s, " ", "_", -1) // Remove spaces
	s = strings.Replace(s, "(", "", -1)  // Remove open parenthesis
	s = strings.Replace(s, ")", "", -1)  // Remove close parenthesis
	s = strings.Replace(s, "/", "", -1)  // Remove forward slashes
	s = strings.Replace(s, "*", "", -1)  // Remove asterisks
	s = strings.ToLower(s)
	return s
}

func main() {
	log.AddFlags(kingpin.CommandLine)
	kingpin.Version("dmdb_exporter " + Version)
	kingpin.HelpFlag.Short('h')
	kingpin.Parse()

	log.Infoln("Starting dmdb_exporter " + Version)
	dsn := os.Getenv("DATA_SOURCE_NAME")
	//dsn := "dm://SYSDBA:SYSDBA@127.0.0.1:5236?autoCommit=true"
	// Load default metrics
	if _, err := toml.DecodeFile(*defaultFileMetrics, &metricsToScrap); err != nil {
		log.Errorln(err)
		panic(errors.New("Error while loading " + *defaultFileMetrics))
	} else {
		log.Infoln("Successfully loaded default metrics from: " + *defaultFileMetrics)
	}

	// If custom metrics, load it
	if strings.Compare(*customMetrics, "") != 0 {
		if _, err := toml.DecodeFile(*customMetrics, &additionalMetrics); err != nil {
			log.Errorln(err)
			panic(errors.New("Error while loading " + *customMetrics))
		} else {
			log.Infoln("Successfully loaded custom metrics from: " + *customMetrics)
		}

		metricsToScrap.Metric = append(metricsToScrap.Metric, additionalMetrics.Metric...)
	} else {
		log.Infoln("No custom metrics defined.")
	}
	if dsn != "" {
		exporter := NewExporter(dsn)
		prometheus.MustRegister(exporter)
	} else {
		var err error
		if exportConf, err = newExporterConfig(*config); err != nil {
			log.Infof("Error parsing config, file: %s, err: %v", *config, err)
			os.Exit(1)
		}
		http.HandleFunc("/scrape", scrapeHandle())
	}
	//exporter := NewExporter(dsn)
	//prometheus.MustRegister(exporter)
	//registry := prometheus.NewRegistry()
	//registry.MustRegister(exporter)
	//http.Handle(*metricPath,  promhttp.Handler())

	// landingPage contains the HTML served at '/'.
	// TODO: Make this nicer and more informative.
	var landingPage = []byte(`<html>
	        <head><title>Dmdb Exporter</title></head>
	        <body>
	        <h1>Dmdb Exporter</h1>
	        <p><a href='` + *metricsPath + `'>Metrics</a></p>
	        </body>
	        </html>`)

	//http.Handle(*metricsPath, promhttp.HandlerFor(registry, promhttp.HandlerOpts{}))

	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.Write(landingPage)
	})
	log.Infoln("Listening on", *listenAddress)
	log.Fatal(http.ListenAndServe(*listenAddress, nil))
}

func scrapeHandle() func(w http.ResponseWriter, r *http.Request) {
	return func(w http.ResponseWriter, r *http.Request) {
		var err error
		target := r.URL.Query().Get("target")
		module := r.URL.Query().Get("module")

		if dsn, err = formExporterDSN(target, module, exportConf); err != nil {
			log.Infof("Error parsing target, target: %s, err: %v", target, err)
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}

		exporter := NewExporter(dsn)
		registry := prometheus.NewRegistry()
		registry.MustRegister(exporter)
		gatherers := prometheus.Gatherers{
			prometheus.DefaultGatherer,
			registry,
		}
		h := promhttp.HandlerFor(gatherers, promhttp.HandlerOpts{})
		h.ServeHTTP(w, r)
	}
}

func formExporterDSN(target string, module string, cfg *ini.File) (string, error) {
	var dsn, host string
	var port uint
	var client *ini.Section
	var err error

	// parse specific target
	if target != "" {
		targetPort := strings.Split(target, ":")
		host = targetPort[0]
		if len(targetPort) > 1 {
			p, err := strconv.ParseUint(targetPort[1], 10, 64)
			if err != nil {
				return "", fmt.Errorf("invalid port %s", targetPort[1])
			}
			port = uint(p)
		}
	}

	// Get client config
	var section string
	if module == "" || module == "default" {
		section = "client"
	} else {
		section = fmt.Sprintf("client.%s", module)
	}

	if client, err = cfg.GetSection(section); err != nil {
		return "", fmt.Errorf("didn't find section [%s] in config", section)
	}

	// default host & port
	if host == "" {
		host = cfg.Section("client").Key("host").MustString("localhost")
	}
	if port == 0 {
		port = cfg.Section("client").Key("port").MustUint(3306)
	}

	user := client.Key("user").String()
	password := client.Key("password").String()
	if (user == "") || (password == "") {
		return dsn, fmt.Errorf("no user or password specified under [%s] in config", section)
	}

	dsn = fmt.Sprintf("dm://%s:%s@%s:%v?autoCommit=true", user, password, host, port)

	return dsn, nil
}

func newExporterConfig(configPath interface{}) (*ini.File, error) {
	opts := ini.LoadOptions{
		// MySQL ini file can have boolean keys.
		AllowBooleanKeys: true,
	}

	var cfg *ini.File
	var err error
	if cfg, err = ini.LoadSources(opts, configPath); err != nil {
		return nil, err
	}

	return cfg, nil
}
