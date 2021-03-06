package main

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"log/syslog"
	"net"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"

	"code.google.com/p/go.net/websocket"
	docker "github.com/fsouza/go-dockerclient"
	"github.com/go-martini/martini"
	elastigo "github.com/mattbaird/elastigo/lib"
)

var debugMode bool

func debug(v ...interface{}) {
	if debugMode {
		log.Println(v...)
	}
}

func assert(err error, context string) {
	if err != nil {
		log.Fatal(context+": ", err)
	}
}

func getopt(name, dfault string) string {
	value := os.Getenv(name)
	if value == "" {
		value = dfault
	}
	return value
}

type Colorizer map[string]int

// returns up to 14 color escape codes (then repeats) for each unique key
func (c Colorizer) Get(key string) string {
	i, exists := c[key]
	if !exists {
		c[key] = len(c)
		i = c[key]
	}
	bright := "1;"
	if i%14 > 6 {
		bright = ""
	}
	return "\x1b[" + bright + "3" + strconv.Itoa(7-(i%7)) + "m"
}

func syslogStreamer(target Target, types []string, logstream chan *Log) {
	typestr := "," + strings.Join(types, ",") + ","
	for logline := range logstream {
		if typestr != ",," && !strings.Contains(typestr, logline.Type) {
			continue
		}
		tag := logline.Name + target.AppendTag
		remote, err := syslog.Dial("udp", target.Addr, syslog.LOG_USER|syslog.LOG_INFO, tag)
		assert(err, "syslog")
		io.WriteString(remote, logline.Data)
	}
}

func udpStreamer(target Target, types []string, logstream chan *Log) {
	typestr := "," + strings.Join(types, ",") + ","
	addr, err := net.ResolveUDPAddr("udp", target.Addr)
	assert(err, "resolve udp failed")
	conn, err := net.DialUDP("udp", nil, addr)
	assert(err, "connect udp failed")
	encoder := json.NewEncoder(conn)
	defer conn.Close()
	for logline := range logstream {
		if typestr != ",," && !strings.Contains(typestr, logline.Type) {
			continue
		}
		encoder.Encode(logline)
	}
}

func elasticsearchStreamer(target Target, types []string, logstream chan *Log) {
	typestr := "," + strings.Join(types, ",") + ","
	c := elastigo.NewConn()
	splitAddr := strings.Split(target.Addr, ":")
	c.Domain = splitAddr[0]
	if len(splitAddr) > 1 {
		c.Port = splitAddr[1]
	}
	indexer := c.NewBulkIndexerErrors(10, 1)
	indexer.BufferDelayMax = 100 * time.Millisecond
	indexer.BulkMaxDocs = 10
	indexer.Start()
	defer indexer.Stop()

	go func() {
		for err := range indexer.ErrorChannel {
			log.Println("Error:", err)
		}
	}()

	if debugMode {
		go func() {
			for {
				log.Println("Number of pending docs:", indexer.PendingDocuments())
				log.Println("Number of errors:", indexer.NumErrors())
				time.Sleep(1 * time.Second)
			}
		}()
	}

	const indexDateStampLayout = "2006.01.02"
	k8sContainerRE := regexp.MustCompile(`^(?:[^_]+)_([^\.]+)\.(?:[^_]+)_([^\.]+)\.([^\.]+)`)
	var tmpMap map[string]interface{}
	for logline := range logstream {
		if typestr != ",," && !strings.Contains(typestr, logline.Type) {
			continue
		}

		k8sContainer := &K8sContainer{}
		match := k8sContainerRE.FindStringSubmatch(logline.Name)
		if len(match) > 0 {
			k8sContainer.Name = match[1]
			k8sContainer.Pod = match[2]
			k8sContainer.Namespace = match[3]
			debug("Found k8s container", k8sContainer)
		} else {
			debug("Not an k8s container", logline.Name)
		}

		now := time.Now()
		index := "logstash-" + now.Format(indexDateStampLayout)
		err := json.Unmarshal([]byte(logline.Data), &tmpMap)
		if err != nil {
			tmpMap = map[string]interface{}{
				"@timestamp": now,
				"message":    logline.Data,
			}
		} else {
			if _, present := tmpMap["@timestamp"]; !present {
				tmpMap["@timestamp"] = now
			}
		}
		tmpMap["container"] = logline.Name
		tmpMap["image"] = logline.Image
		if len(k8sContainer.Pod) > 0 {
			tmpMap["k8s_pod"] = k8sContainer.Pod
			tmpMap["k8s_container"] = k8sContainer.Name
			tmpMap["k8s_namespace"] = k8sContainer.Namespace
		}
		indexer.Index(index, "log", "", "", &now, tmpMap, false)
		if debugMode {
			log.Println("Indexed", tmpMap)
		}
	}
}

func websocketStreamer(w http.ResponseWriter, req *http.Request, logstream chan *Log, closer chan bool) {
	websocket.Handler(func(conn *websocket.Conn) {
		for logline := range logstream {
			if req.URL.Query().Get("type") != "" && logline.Type != req.URL.Query().Get("type") {
				continue
			}
			_, err := conn.Write(append(marshal(logline), '\n'))
			if err != nil {
				closer <- true
				return
			}
		}
	}).ServeHTTP(w, req)
}

func httpStreamer(w http.ResponseWriter, req *http.Request, logstream chan *Log, multi bool) {
	var colors Colorizer
	var usecolor, usejson bool
	nameWidth := 16
	if req.URL.Query().Get("colors") != "off" {
		colors = make(Colorizer)
		usecolor = true
	}
	if req.Header.Get("Accept") == "application/json" {
		w.Header().Add("Content-Type", "application/json")
		usejson = true
	} else {
		w.Header().Add("Content-Type", "text/plain")
	}
	for logline := range logstream {
		if req.URL.Query().Get("types") != "" && logline.Type != req.URL.Query().Get("types") {
			continue
		}
		if usejson {
			w.Write(append(marshal(logline), '\n'))
		} else {
			if multi {
				if len(logline.Name) > nameWidth {
					nameWidth = len(logline.Name)
				}
				if usecolor {
					w.Write([]byte(fmt.Sprintf(
						"%s%"+strconv.Itoa(nameWidth)+"s|%s\x1b[0m\n",
						colors.Get(logline.Name), logline.Name, logline.Data,
					)))
				} else {
					w.Write([]byte(fmt.Sprintf(
						"%"+strconv.Itoa(nameWidth)+"s|%s\n", logline.Name, logline.Data,
					)))
				}
			} else {
				w.Write(append([]byte(logline.Data), '\n'))
			}
		}
		w.(http.Flusher).Flush()
	}
}

func main() {
	debugMode = getopt("DEBUG", "") != ""
	port := getopt("PORT", "8000")
	endpoint := getopt("DOCKER_HOST", "unix:///var/run/docker.sock")
	routespath := getopt("ROUTESPATH", "/var/lib/logspout")

	client, err := docker.NewClient(endpoint)
	assert(err, "docker")
	attacher := NewAttachManager(client)
	router := NewRouteManager(attacher)

	if len(os.Args) > 1 {
		expandedUrl := os.ExpandEnv(os.Args[1])
		u, err := url.Parse(expandedUrl)
		assert(err, "url")
		log.Println("routing all to " + expandedUrl)
		router.Add(&Route{Target: Target{Type: u.Scheme, Addr: u.Host}})
	}

	if _, err := os.Stat(routespath); err == nil {
		log.Println("loading and persisting routes in " + routespath)
		assert(router.Load(RouteFileStore(routespath)), "persistor")
	}

	m := martini.Classic()

	m.Get("/logs(?:/(?P<predicate>[a-zA-Z]+):(?P<value>.+))?", func(w http.ResponseWriter, req *http.Request, params martini.Params) {
		source := new(Source)
		switch {
		case params["predicate"] == "id" && params["value"] != "":
			source.ID = params["value"][:12]
		case params["predicate"] == "name" && params["value"] != "":
			source.Name = params["value"]
		case params["predicate"] == "filter" && params["value"] != "":
			source.Filter = params["value"]
		}

		if source.ID != "" && attacher.Get(source.ID) == nil {
			http.NotFound(w, req)
			return
		}

		logstream := make(chan *Log)
		defer close(logstream)

		var closer <-chan bool
		if req.Header.Get("Upgrade") == "websocket" {
			closerBi := make(chan bool)
			go websocketStreamer(w, req, logstream, closerBi)
			closer = closerBi
		} else {
			go httpStreamer(w, req, logstream, source.All() || source.Filter != "")
			closer = w.(http.CloseNotifier).CloseNotify()
		}

		attacher.Listen(source, logstream, closer)
	})

	m.Get("/routes", func(w http.ResponseWriter, req *http.Request) {
		w.Header().Add("Content-Type", "application/json")
		routes, _ := router.GetAll()
		w.Write(append(marshal(routes), '\n'))
	})

	m.Post("/routes", func(w http.ResponseWriter, req *http.Request) (int, string) {
		route := new(Route)
		if err := unmarshal(req.Body, route); err != nil {
			return http.StatusBadRequest, "Bad request: " + err.Error()
		}

		// TODO: validate?
		router.Add(route)

		w.Header().Add("Content-Type", "application/json")
		return http.StatusCreated, string(append(marshal(route), '\n'))
	})

	m.Get("/routes/:id", func(w http.ResponseWriter, req *http.Request, params martini.Params) {
		route, _ := router.Get(params["id"])
		if route == nil {
			http.NotFound(w, req)
			return
		}
		w.Write(append(marshal(route), '\n'))
	})

	m.Delete("/routes/:id", func(w http.ResponseWriter, req *http.Request, params martini.Params) {
		if ok := router.Remove(params["id"]); !ok {
			http.NotFound(w, req)
		}
	})

	log.Println("logspout serving http on :" + port)
	log.Fatal(http.ListenAndServe(":"+port, m))
}
