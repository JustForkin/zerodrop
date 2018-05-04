package main

import (
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/client9/ipcat"

	"github.com/oschwald/geoip2-golang"
)

var headerCacheControl = "no-cache, no-store, must-revalidate"

// ShotHandler serves the requested page and removes it from the database, or
// returns 404 page if not available.
type ShotHandler struct {
	DB       *ZerodropDB
	Config   *ZerodropConfig
	NotFound NotFoundHandler
	Context  *BlacklistContext
}

// NewShotHandler constructs a new ShotHandler from the arguments.
func NewShotHandler(db *ZerodropDB, config *ZerodropConfig, notfound NotFoundHandler) *ShotHandler {
	var geodb *geoip2.Reader
	if config.GeoDB != "" {
		var err error
		geodb, err = geoip2.Open(config.GeoDB)
		if err != nil {
			log.Printf("Could not open geolocation database: %s", err.Error())
		}
	}

	var ipset *ipcat.IntervalSet
	if config.IPCat != "" {
		reader, err := os.Open(config.IPCat)
		if err != nil {
			log.Printf("Could not open ipcat database: %s", err.Error())
		} else {
			ipset = ipcat.NewIntervalSet(4096)
			err := ipset.ImportCSV(reader)
			if err != nil {
				log.Printf("Could not import ipcat database: %s", err.Error())
				ipset = nil
			}
		}
	}

	return &ShotHandler{
		DB:       db,
		Config:   config,
		NotFound: notfound,
		Context: &BlacklistContext{
			GeoDB: geodb,
			IPSet: ipset,
		},
	}
}

// Access returns the ZerodropEntry with the specified name as long as access
// is permitted. The function returns nil otherwise.
func (a *ShotHandler) Access(name string, request *http.Request) *ZerodropEntry {
	addr := RealRemoteAddr(request)

	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		log.Printf("Could not parse remote address and port: %s", err.Error())
		return nil
	}

	ip := net.ParseIP(host)
	if ip == nil {
		log.Printf("Could not parse IP address: %s", host)
		return nil
	}

	entry, ok := a.DB.Get(name)
	if !ok {
		return nil
	}

	if entry.AccessTrain {
		date := time.Now().Format(time.RFC1123)
		entry.AccessBlacklist.Add(&BlacklistRule{Comment: "Automatically added by training on " + date})

		// We need to add the ip to the blacklist
		entry.AccessBlacklist.Add(&BlacklistRule{IP: ip})

		// We will also add the Geofence
		if a.Context.GeoDB != nil {
			record, err := a.Context.GeoDB.City(ip)
			if err == nil {
				entry.AccessBlacklist.Add(&BlacklistRule{
					Geofence: &Geofence{
						Latitude:  record.Location.Latitude,
						Longitude: record.Location.Longitude,
						Radius:    float64(record.Location.AccuracyRadius) * 1000.0, // Convert km to m
					},
				})
			}
		}

		if err := entry.Update(); err != nil {
			log.Printf("Error adding to blacklist: %s", err.Error())
			return nil
		}
		return nil
	}

	if entry.IsExpired() {
		log.Printf("Access restricted to expired %s from %s", entry.Name, ip.String())
		entry.AccessBlacklistCount++
		entry.Update()
		return nil
	}

	if !entry.AccessBlacklist.Allow(a.Context, ip) {
		log.Printf("Access restricted to %s from blacklisted %s", entry.Name, ip.String())
		entry.AccessBlacklistCount++
		entry.Update()
		return nil
	}

	entry.AccessCount++
	entry.Update()

	return &entry
}

// ServeHTTP generates the HTTP response.
func (a *ShotHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// Get entry
	name := strings.Trim(r.URL.Path, "/")
	entry := a.Access(name, r)

	if entry == nil {
		log.Printf("Denied access to %s to %s", strconv.Quote(name), RealRemoteAddr(r))
		a.NotFound.ServeHTTP(w, r)
		return
	}

	log.Printf("Granted access to %s to %s", strconv.Quote(entry.Name), RealRemoteAddr(r))

	// File Upload
	if entry.URL == "" {
		contentType := entry.ContentType
		if contentType == "" {
			contentType = "text/plain"
		}

		fullpath := filepath.Join(a.Config.UploadDirectory, entry.Filename)
		file, err := os.Open(fullpath)
		if err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
		defer file.Close()

		w.Header().Set("Content-Type", contentType)
		w.Header().Set("Cache-Control", headerCacheControl)
		io.Copy(w, file)
		return
	}

	// URL redirect
	if entry.Redirect {
		// Perform a redirect to the URL.
		w.Header().Set("Cache-Control", headerCacheControl)
		http.Redirect(w, r, entry.URL, 307)
		return
	}

	// URL proxy
	target, err := url.Parse(entry.URL)
	if err != nil {
		http.Error(w, "Could not parse URL", 500)
		return
	}

	proxy := &httputil.ReverseProxy{
		Director: func(req *http.Request) {
			req.URL = target
			req.Host = target.Host
			if _, ok := req.Header["User-Agent"]; !ok {
				// explicitly disable User-Agent so it's not set to default value
				req.Header.Set("User-Agent", "")
			}
		},
		ModifyResponse: func(res *http.Response) error {
			w.Header().Set("Cache-Control", headerCacheControl)
			return nil
		},
	}

	proxy.ServeHTTP(w, r)
}
