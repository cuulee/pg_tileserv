package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"net/http"
	"os"
	"time"

	// REST routing
	"github.com/gorilla/handlers"
	"github.com/gorilla/mux"

	// Database connectivity
	"github.com/jackc/pgx/v4/pgxpool"

	// Logging
	log "github.com/sirupsen/logrus"

	// Configuration
	"github.com/pborman/getopt/v2"
	"github.com/spf13/viper"
)

// programName is the name string we use
const programName string = "pg_tileserv"

// programVersion is the version string we use
const programVersion string = "0.1"

// worldMercWidth is the width of the Web Mercator plane
const worldMercWidth float64 = 40075016.6855784

// globalDb is a global database connection pointer
var globalDb *pgxpool.Pool = nil

/******************************************************************************/

func Calculate(i int) int {
	return 3
}

func main() {

	viper.SetDefault("DbConnection", "sslmode=disable")
	viper.SetDefault("HttpHost", "0.0.0.0")
	viper.SetDefault("HttpPort", 7800)
	viper.SetDefault("UrlBase", "")
	viper.SetDefault("DefaultResolution", 4096)
	viper.SetDefault("DefaultBuffer", 256)
	viper.SetDefault("MaxFeaturesPerTile", 50000)
	viper.SetDefault("DefaultMinZoom", 0)
	viper.SetDefault("DefaultMaxZoom", 22)
	viper.SetDefault("Debug", false)
	viper.SetDefault("AssetsPath", "./assets")

	// Read environment configuration first
	if dbUrl := os.Getenv("DATABASE_URL"); dbUrl != "" {
		viper.Set("DbConnection", dbUrl)
	}

	// Read the commandline
	flagDebugOn := getopt.BoolLong("debug", 'd', "log debugging information")
	flagConfigFile := getopt.StringLong("config", 'c', "", "config file name")
	flagHelpOn := getopt.BoolLong("help", 'h', "display help output")
	getopt.Parse()

	if *flagHelpOn {
		getopt.PrintUsage(os.Stdout)
		os.Exit(1)
	}

	if *flagConfigFile != "" {
		viper.SetConfigFile(*flagConfigFile)
	} else {
		viper.SetConfigName("config")
		viper.AddConfigPath(fmt.Sprintf("/etc/%s", programName))
		viper.AddConfigPath(fmt.Sprintf("$HOME/.%s", programName))
		viper.AddConfigPath(".")
	}

	if err := viper.ReadInConfig(); err != nil {
		if _, ok := err.(viper.ConfigFileNotFoundError); ok {
			log.Debug(err)
		} else {
			log.Fatal(err)
		}
	}

	// Commandline over-rides config file for debugging
	if *flagDebugOn {
		viper.Set("Debug", true)
		log.SetLevel(log.TraceLevel)
	}

	// Report our status
	log.Infof("%s %s\n", programName, programVersion)
	log.Info("Run with --help parameter for commandline options\n")
	log.Infof("Listening on: %s:%d", viper.GetString("HttpHost"), viper.GetInt("HttpPort"))

	// Load the global layer list right away
	// Also connects to database
	if err := LoadLayers(); err != nil {
		log.Fatal(err)
	}

	// Get to work
	handleRequests()
}

/******************************************************************************/

func requestPreview(w http.ResponseWriter, r *http.Request) error {
	lyrId := mux.Vars(r)["name"]
	log.WithFields(log.Fields{
		"event": "request",
		"topic": "layerpreview",
		"key":   lyrId,
	}).Tracef("requestPreview: %s", lyrId)

	// Refresh the layers list
	if err := LoadLayers(); err != nil {
		return err
	}
	// Get the requested layer
	lyr, errLyr := GetLayer(lyrId)
	if errLyr != nil {
		return errLyr
	}

	switch lyr.(type) {
	case LayerTable:
		tmpl, err := template.ParseFiles(fmt.Sprintf("%s/preview-table.html", viper.GetString("AssetsPath")))
		if err != nil {
			return err
		}
		l, _ := lyr.(LayerTable)
		tmpl.Execute(w, l)
	case LayerFunction:
		tmpl, err := template.ParseFiles(fmt.Sprintf("%s/preview-function.html", viper.GetString("AssetsPath")))
		if err != nil {
			return err
		}
		l, _ := lyr.(LayerFunction)
		tmpl.Execute(w, l)
	default:
		return errors.New("unknown layer type") // never get here
	}
	return nil
}

func requestListHtml(w http.ResponseWriter, r *http.Request) error {
	log.WithFields(log.Fields{
		"event": "request",
		"topic": "layerlist",
	}).Trace("requestListHtml")
	// Update the global in-memory list from
	// the database
	if err := LoadLayers(); err != nil {
		return err
	}
	jsonLayers := GetJsonLayers(r)
	t, err := template.ParseFiles(fmt.Sprintf("%s/index.html", viper.GetString("AssetsPath")))
	if err != nil {
		log.Warn(err)
	}
	t.Execute(w, jsonLayers)
	return nil
}

func requestListJson(w http.ResponseWriter, r *http.Request) error {
	log.WithFields(log.Fields{
		"event": "request",
		"topic": "layerlist",
	}).Trace("requestListJson")
	// Update the global in-memory list from
	// the database
	if err := LoadLayers(); err != nil {
		return err
	}
	w.Header().Add("Content-Type", "application/json")
	jsonLayers := GetJsonLayers(r)
	json.NewEncoder(w).Encode(jsonLayers)
	return nil
}

func requestDetailJson(w http.ResponseWriter, r *http.Request) error {
	lyrId := mux.Vars(r)["name"]
	log.WithFields(log.Fields{
		"event": "request",
		"topic": "layerdetail",
	}).Tracef("requestDetailJson(%s)", lyrId)

	// Refresh the layers list
	if err := LoadLayers(); err != nil {
		return err
	}

	lyr, errLyr := GetLayer(lyrId)
	if errLyr != nil {
		return errLyr
	}

	errWrite := lyr.WriteLayerJson(w, r)
	if errWrite != nil {
		return errWrite
	}
	return nil
}

func requestTile(w http.ResponseWriter, r *http.Request) error {
	vars := mux.Vars(r)
	lyr, errLyr := GetLayer(vars["name"])
	if errLyr != nil {
		return errLyr
	}
	tile, errTile := makeTile(vars)
	if errTile != nil {
		return errTile
	}

	log.WithFields(log.Fields{
		"event": "request",
		"topic": "tile",
		"key":   tile.String(),
	}).Tracef("RequestLayerTile: %s", tile.String())

	tilerequest := lyr.GetTileRequest(tile, r)
	mvt, errMvt := DBTileRequest(&tilerequest)
	if errMvt != nil {
		return errMvt
	}

	if _, errWrite := w.Write(mvt); errWrite != nil {
		return errWrite
	}
	w.Header().Add("Content-Type", "application/vnd.mapbox-vector-tile")

	return nil
}

/******************************************************************************/

// tileAppError is an optional error structure functions can return
// if they want to specify the particular HTTP error code to be used
// in their error return
type tileAppError struct {
	HttpCode int
	SrcErr   error
	Topic    string
	Message  string
}

// Error prints out a reasonable string format
func (tae tileAppError) Error() string {
	if tae.Message != "" {
		return fmt.Sprintf("%s\n%s", tae.Message, tae.SrcErr.Error())
	}
	return fmt.Sprintf("%s", tae.SrcErr.Error())
}

// tileAppHandler is a function handler that can replace the
// existing handler and provide richer error handling, see below and
// https://blog.golang.org/error-handling-and-go
type tileAppHandler func(w http.ResponseWriter, r *http.Request) error

// ServeHTTP logs as much useful information as possible in
// a field format for potential Json logging streams
// as well as returning HTTP error response codes on failure
// so clients can see what is going on
// TODO: return JSON document body for the HTTP error
func (fn tileAppHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	log.WithFields(log.Fields{
		"method": r.Method,
		"url":    r.URL,
	}).Infof("%s %s", r.Method, r.URL)
	if err := fn(w, r); err != nil {
		if hdr, ok := r.Header["x-correlation-id"]; ok {
			log.WithField("correlation-id", hdr[0])
		}
		if e, ok := err.(tileAppError); ok {
			if e.HttpCode == 0 {
				e.HttpCode = 500
			}
			if e.Topic != "" {
				log.WithField("topic", e.Topic)
			}
			log.WithField("key", e.Message)
			log.WithField("src", e.SrcErr.Error())
			log.Error(err)
			http.Error(w, e.Error(), e.HttpCode)
		} else {
			log.Error(err)
			http.Error(w, err.Error(), 500)
		}
	}
}

/******************************************************************************/

func handleRequests() {

	// creates a new instance of a mux router
	r := mux.NewRouter().StrictSlash(true)
	r.Handle("/", tileAppHandler(requestListHtml))
	r.Handle("/index.html", tileAppHandler(requestListHtml))
	r.Handle("/{name}.html", tileAppHandler(requestPreview))
	r.Handle("/index.json", tileAppHandler(requestListJson))
	r.Handle("/{name}.json", tileAppHandler(requestDetailJson))
	r.Handle("/{name}/{z:[0-9]+}/{x:[0-9]+}/{y:[0-9]+}.{ext}", tileAppHandler(requestTile))

	// Allow CORS from anywhere
	corsOpt := handlers.AllowedOrigins([]string{"*"})

	// more "production friendly" timeouts
	// https://blog.simon-frey.eu/go-as-in-golang-standard-net-http-config-will-break-your-production/#You_should_at_least_do_this_The_easy_path
	s := &http.Server{
		ReadTimeout:  1 * time.Second,
		WriteTimeout: 10 * time.Second,
		Addr:         fmt.Sprintf("%s:%d", viper.GetString("HttpHost"), viper.GetInt("HttpPort")),
		Handler:      handlers.CompressHandler(handlers.CORS(corsOpt)(r)),
	}

	// TODO figure out how to gracefully shut down on ^C
	// and shut down all the database connections / statements
	log.Fatal(s.ListenAndServe())
}

/******************************************************************************/
