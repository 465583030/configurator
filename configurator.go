package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"net/http"
	"net/url"
	"os"
	"strings"
)

var port = flag.String("p", "8881", "port to listen on")
var checkCmd = flag.String("c", "", "config check command. FILE set in env")
var reloadCmd = flag.String("r", "", "reload command")

func assert(err error) {
	if err != nil {
		log.Fatal(err)
	}
}

func marshal(obj interface{}) []byte {
	bytes, err := json.MarshalIndent(obj, "", "  ")
	if err != nil {
		log.Println("marshal:", err)
	}
	return bytes
}

func unmarshal(input io.ReadCloser, obj interface{}) error {
	body, err := ioutil.ReadAll(input)
	if err != nil {
		return err
	}
	err = json.Unmarshal(body, obj)
	if err != nil {
		return err
	}
	return nil
}

func init() {
	flag.Usage = func() {
		fmt.Fprintf(os.Stderr, "Usage: %v [options] <configstore-uri> <transformer> <target-file>\n\n", os.Args[0])
		flag.PrintDefaults()
	}
}

func main() {
	flag.Parse()
	if flag.NArg() < 3 {
		flag.Usage()
		os.Exit(64)
	}

	uri, err := url.Parse(flag.Arg(0))
	assert(err)
	factory := map[string]func(*url.URL) (*ConsulStore, error){
		"consul": NewConsulStore,
	}[uri.Scheme]
	if factory == nil {
		log.Fatal("Unrecognized config store backend: ", uri.Scheme)
	}

	store, err := factory(uri)
	assert(err)

	transformer := flag.Arg(1)
	target := flag.Arg(2)

	config, err := NewConfig(store, target, transformer, *reloadCmd, *checkCmd)
	assert(err)

	log.Printf("Pulling and validating from %s...\n", flag.Arg(0))
	err = config.Update()
	if e, ok := err.(*ExecError); ok {
		fmt.Printf("!! Initial pull from config store resulted in validation error.\n")
		fmt.Printf("!! Output of '%s':\n", *checkCmd)
		fmt.Println(e.Output)
		os.Exit(3)
	} else {
		assert(err)
	}

	http.HandleFunc("/v1/render", func(w http.ResponseWriter, req *http.Request) {
		log.Println(req.Method, req.RequestURI)
		switch req.Method {
		case "GET":
			w.Write(config.LastRender())
		case "POST":
			body, err := ioutil.ReadAll(req.Body)
			if err != nil {
				w.WriteHeader(http.StatusBadRequest)
				io.WriteString(w, "Bad request: "+err.Error())
				return
			}
			newconfig := config.Copy()
			err = newconfig.Load(body)
			if err != nil {
				w.WriteHeader(http.StatusBadRequest)
				io.WriteString(w, "Bad request: "+err.Error())
				return
			}
			err = newconfig.Validate()
			if err != nil {
				w.WriteHeader(http.StatusBadRequest)
				if e, ok := err.(*ExecError); ok {
					io.WriteString(w, e.Output)
					io.WriteString(w, e.Input)
				} else {
					io.WriteString(w, err.Error())
				}
				return
			}
			w.Write(newconfig.LastRender())

		default:
			w.WriteHeader(http.StatusMethodNotAllowed)
		}
	})

	http.HandleFunc("/v1/config/", func(w http.ResponseWriter, req *http.Request) {
		log.Println(req.Method, req.RequestURI)
		path := strings.TrimPrefix(req.RequestURI, "/v1/config")
		handleMutateError := func(err error) {
			if err != nil {
				log.Println(err)
				w.WriteHeader(http.StatusBadRequest)
				if e, ok := err.(*ExecError); ok {
					io.WriteString(w, e.Output)
				} else {
					io.WriteString(w, err.Error())
				}
			}
		}
		readJsonBody := func() interface{} {
			var json interface{}
			if err := unmarshal(req.Body, &json); err != nil {
				w.WriteHeader(http.StatusBadRequest)
				io.WriteString(w, "Bad request: "+err.Error())
				return nil
			}
			return json
		}
		switch req.Method {
		case "GET":
			w.Header().Add("Content-Type", "application/json")
			w.Write(append(marshal(config.Get(path)), '\n'))
		case "POST":
			if !config.Tree().IsComposite(path) {
				w.WriteHeader(http.StatusMethodNotAllowed)
				return
			}
			json := readJsonBody()
			if json == nil {
				return
			}
			obj, isObj := json.(map[string]interface{})
			err := config.Mutate(func(c *JsonTree) bool {
				if c.IsObject(path) && isObj {
					return c.Merge(path, obj)
				} else {
					return c.Append(path, json)
				}
			})
			handleMutateError(err)
		case "PUT":
			json := readJsonBody()
			if json == nil {
				return
			}
			err := config.Mutate(func(c *JsonTree) bool {
				return c.Replace(path, json)
			})
			handleMutateError(err)
		case "DELETE":
			err := config.Mutate(func(c *JsonTree) bool {
				return c.Delete(path)
			})
			handleMutateError(err)
		default:
			w.WriteHeader(http.StatusMethodNotAllowed)
		}
	})
	log.Println("Listening on port " + *port)
	log.Fatal(http.ListenAndServe(":"+*port, nil))

}
