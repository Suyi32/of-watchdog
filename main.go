// Copyright (c) OpenFaaS Author(s) 2021. All rights reserved.
// Licensed under the MIT license. See LICENSE file in the project root for full license information.

package main

import (
	"context"
	"flag"
	"fmt"
	"io/ioutil"
	"log"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync/atomic"
	"syscall"
	"time"
	"bufio"
	"strconv"

	limiter "github.com/openfaas/faas-middleware/concurrency-limiter"
	"github.com/openfaas/of-watchdog/config"
	"github.com/openfaas/of-watchdog/executor"
	"github.com/openfaas/of-watchdog/metrics"
)

var (
	acceptingConnections int32
)

func main() {
	var runHealthcheck bool
	var versionFlag bool

	flag.BoolVar(&versionFlag, "version", false, "Print the version and exit")
	flag.BoolVar(&runHealthcheck,
		"run-healthcheck",
		false,
		"Check for the a lock-file, when using an exec healthcheck. Exit 0 for present, non-zero when not found.")

	flag.Parse()

	printVersion()

	if versionFlag {
		return
	}

	if runHealthcheck {
		if lockFilePresent() {
			os.Exit(0)
		}

		fmt.Fprintf(os.Stderr, "unable to find lock file.\n")
		os.Exit(1)
	}

	atomic.StoreInt32(&acceptingConnections, 0)

	watchdogConfig := config.New(os.Environ())

	if len(watchdogConfig.FunctionProcess) == 0 && watchdogConfig.OperationalMode != config.ModeStatic {
		fmt.Fprintf(os.Stderr, "Provide a \"function_process\" or \"fprocess\" environmental variable for your function.\n")
		os.Exit(1)
	}

	requestHandler := buildRequestHandler(watchdogConfig, watchdogConfig.PrefixLogs)

	log.Printf("Watchdog mode: %s\n", config.WatchdogMode(watchdogConfig.OperationalMode))

	httpMetrics := metrics.NewHttp()
	http.HandleFunc("/", metrics.InstrumentHandler(requestHandler, httpMetrics))
	http.HandleFunc("/_/health", makeHealthHandler())

	metricsServer := metrics.MetricsServer{}
	metricsServer.Register(watchdogConfig.MetricsPort)

	cancel := make(chan bool)

	go metricsServer.Serve(cancel)

	shutdownTimeout := watchdogConfig.HTTPWriteTimeout
	s := &http.Server{
		Addr:           fmt.Sprintf(":%d", watchdogConfig.TCPPort),
		ReadTimeout:    watchdogConfig.HTTPReadTimeout,
		WriteTimeout:   watchdogConfig.HTTPWriteTimeout,
		MaxHeaderBytes: 1 << 20, // Max header of 1MB
	}

	log.Printf("Timeouts: read: %s, write: %s hard: %s.\n",
		watchdogConfig.HTTPReadTimeout,
		watchdogConfig.HTTPWriteTimeout,
		watchdogConfig.ExecTimeout)

	log.Printf("Listening on port: %d\n", watchdogConfig.TCPPort)

	listenUntilShutdown(shutdownTimeout, s, watchdogConfig.SuppressLock)
}

func markUnhealthy() error {
	atomic.StoreInt32(&acceptingConnections, 0)

	path := filepath.Join(os.TempDir(), ".lock")
	log.Printf("Removing lock-file : %s\n", path)
	removeErr := os.Remove(path)
	return removeErr
}

func listenUntilShutdown(shutdownTimeout time.Duration, s *http.Server, suppressLock bool) {

	idleConnsClosed := make(chan struct{})
	go func() {
		sig := make(chan os.Signal, 1)
		signal.Notify(sig, syscall.SIGTERM)

		<-sig

		log.Printf("SIGTERM received.. shutting down server in %s\n", shutdownTimeout.String())

		healthErr := markUnhealthy()

		if healthErr != nil {
			log.Printf("Unable to mark unhealthy during shutdown: %s\n", healthErr.Error())
		}

		<-time.Tick(shutdownTimeout)

		if err := s.Shutdown(context.Background()); err != nil {
			// Error from closing listeners, or context timeout:
			log.Printf("Error in Shutdown: %v", err)
		}

		log.Printf("No new connections allowed. Exiting in: %s\n", shutdownTimeout.String())

		<-time.Tick(shutdownTimeout)

		close(idleConnsClosed)
	}()

	// Run the HTTP server in a separate go-routine.
	go func() {
		if err := s.ListenAndServe(); err != http.ErrServerClosed {
			log.Printf("Error ListenAndServe: %v", err)
			close(idleConnsClosed)
		}
	}()

	if suppressLock == false {
		path, writeErr := createLockFile()

		if writeErr != nil {
			log.Panicf("Cannot write %s. To disable lock-file set env suppress_lock=true.\n Error: %s.\n", path, writeErr.Error())
		}
	} else {
		log.Println("Warning: \"suppress_lock\" is enabled. No automated health-checks will be in place for your function.")

		atomic.StoreInt32(&acceptingConnections, 1)
	}

	<-idleConnsClosed
}

func buildRequestHandler(watchdogConfig config.WatchdogConfig, prefixLogs bool) http.Handler {
	var requestHandler http.HandlerFunc

	switch watchdogConfig.OperationalMode {
	case config.ModeStreaming:
		requestHandler = makeForkRequestHandler(watchdogConfig, prefixLogs)
		break
	case config.ModeSerializing:
		requestHandler = makeSerializingForkRequestHandler(watchdogConfig, prefixLogs)
		break
	case config.ModeHTTP:
		requestHandler = makeHTTPRequestHandler(watchdogConfig, prefixLogs)
		break
	case config.ModeStatic:
		requestHandler = makeStaticRequestHandler(watchdogConfig)
	default:
		log.Panicf("unknown watchdog mode: %d", watchdogConfig.OperationalMode)
		break
	}

	if watchdogConfig.MaxInflight > 0 {
		return limiter.NewConcurrencyLimiter(requestHandler, watchdogConfig.MaxInflight)
	}

	return requestHandler
}

// createLockFile returns a path to a lock file and/or an error
// if the file could not be created.
func createLockFile() (string, error) {
	path := filepath.Join(os.TempDir(), ".lock")
	log.Printf("Writing lock-file to: %s\n", path)

	mkdirErr := os.MkdirAll(os.TempDir(), os.ModePerm)
	if mkdirErr != nil {
		return path, mkdirErr
	}

	writeErr := ioutil.WriteFile(path, []byte{}, 0660)
	if writeErr != nil {
		return path, writeErr
	}

	atomic.StoreInt32(&acceptingConnections, 1)
	return path, nil
}

func makeSerializingForkRequestHandler(watchdogConfig config.WatchdogConfig, logPrefix bool) func(http.ResponseWriter, *http.Request) {
	functionInvoker := executor.SerializingForkFunctionRunner{
		ExecTimeout: watchdogConfig.ExecTimeout,
		LogPrefix:   logPrefix,
	}

	return func(w http.ResponseWriter, r *http.Request) {

		var environment []string

		if watchdogConfig.InjectCGIHeaders {
			environment = getEnvironment(r)
		}

		commandName, arguments := watchdogConfig.Process()
		req := executor.FunctionRequest{
			Process:       commandName,
			ProcessArgs:   arguments,
			InputReader:   r.Body,
			ContentLength: &r.ContentLength,
			OutputWriter:  w,
			Environment:   environment,
		}

		w.Header().Set("Content-Type", watchdogConfig.ContentType)
		err := functionInvoker.Run(req, w)
		if err != nil {
			log.Println(err)
		}
	}
}

func makeForkRequestHandler(watchdogConfig config.WatchdogConfig, prefixLogs bool) func(http.ResponseWriter, *http.Request) {
	functionInvoker := executor.ForkFunctionRunner{
		ExecTimeout: watchdogConfig.ExecTimeout,
		LogPrefix:   prefixLogs,
	}

	return func(w http.ResponseWriter, r *http.Request) {

		var environment []string

		if watchdogConfig.InjectCGIHeaders {
			environment = getEnvironment(r)
		}

		commandName, arguments := watchdogConfig.Process()
		req := executor.FunctionRequest{
			Process:      commandName,
			ProcessArgs:  arguments,
			InputReader:  r.Body,
			OutputWriter: w,
			Environment:  environment,
		}

		w.Header().Set("Content-Type", watchdogConfig.ContentType)
		err := functionInvoker.Run(req)
		if err != nil {
			log.Println(err.Error())

			// Probably cannot write to client if we already have written a header
			// w.WriteHeader(500)
			// w.Write([]byte(err.Error()))
		}
	}
}

func getEnvironment(r *http.Request) []string {
	var envs []string

	envs = os.Environ()
	for k, v := range r.Header {
		kv := fmt.Sprintf("Http_%s=%s", strings.Replace(k, "-", "_", -1), v[0])
		envs = append(envs, kv)
	}
	envs = append(envs, fmt.Sprintf("Http_Method=%s", r.Method))

	if len(r.URL.RawQuery) > 0 {
		envs = append(envs, fmt.Sprintf("Http_Query=%s", r.URL.RawQuery))
	}

	if len(r.URL.Path) > 0 {
		envs = append(envs, fmt.Sprintf("Http_Path=%s", r.URL.Path))
	}

	if len(r.TransferEncoding) > 0 {
		envs = append(envs, fmt.Sprintf("Http_Transfer_Encoding=%s", r.TransferEncoding[0]))
	}

	return envs
}

func readMemo() float32 {
        var memo_usage_in_bytes float32
        var cache float32

        file, err := os.Open("/sys/fs/cgroup/memory/memory.usage_in_bytes")
        if err !=  nil{
                log.Fatal(err)
        }
        defer file.Close()
        scanner := bufio.NewScanner(file)
        for scanner.Scan() {
                _, err := fmt.Sscan(scanner.Text(), &memo_usage_in_bytes)
                //strconv.ParseFloat(f, 32)
                if err != nil{
                        log.Fatal(err)
                }
        }
        if err := scanner.Err(); err != nil{
                log.Fatal(err)
        }
        // fmt.Printf("memo=%f, type: %T\n", memo_usage_in_bytes, memo_usage_in_bytes)

        file, err = os.Open("/sys/fs/cgroup/memory/memory.stat")
        if err != nil{
                log.Fatal(err)
        }
        defer file.Close()
        scanner = bufio.NewScanner(file)
        for scanner.Scan() {
            split := strings.Split(scanner.Text(), " ")
            // fmt.Println(split)
            if split[0] == "cache"{
                _, err = fmt.Sscan(split[1], &cache)
            }
        }
        // fmt.Printf("cache=%f, type: %T\n", cache, cache)

        return (memo_usage_in_bytes - cache) / 1048576.0
}

func readCPU() float64 {
        data, err := ioutil.ReadFile("/sys/fs/cgroup/cpuacct/cpuacct.usage")
        if err != nil{
                log.Fatal(err)
        }
        cpu_string := strings.TrimSuffix(string(data), "\n")
        cpu_usage, err := strconv.ParseFloat(cpu_string, 64)
        //fmt.Println(cpu_usage, err, reflect.TypeOf(cpu_usage))
        return float64(cpu_usage)
}

func makeHTTPRequestHandler(watchdogConfig config.WatchdogConfig, prefixLogs bool) func(http.ResponseWriter, *http.Request) {
	commandName, arguments := watchdogConfig.Process()
	functionInvoker := executor.HTTPFunctionRunner{
		ExecTimeout:    watchdogConfig.ExecTimeout,
		Process:        commandName,
		ProcessArgs:    arguments,
		BufferHTTPBody: watchdogConfig.BufferHTTPBody,
		LogPrefix:      prefixLogs,
	}

	if len(watchdogConfig.UpstreamURL) == 0 {
		log.Fatal(`For "mode=http" you must specify a valid URL for "http_upstream_url"`)
	}

	urlValue, upstreamURLErr := url.Parse(watchdogConfig.UpstreamURL)
	if upstreamURLErr != nil {
		log.Fatal(upstreamURLErr)
	}
	functionInvoker.UpstreamURL = urlValue

	log.Printf("Forking: %s, arguments: %s", commandName, arguments)
	functionInvoker.Start()

	return func(w http.ResponseWriter, r *http.Request) {

		stopCh := make(chan bool)
		var new_memo float32
		var max_memo float32

		req := executor.FunctionRequest{
			Process:      commandName,
			ProcessArgs:  arguments,
			InputReader:  r.Body,
			OutputWriter: w,
		}

		if r.Body != nil {
			defer r.Body.Close()
		}

		cpu_start := readCPU()
		go func(){
			err := functionInvoker.Run(req, r.ContentLength, r, w, stopCh)
			if err != nil {
				w.WriteHeader(500)
				w.Write([]byte(err.Error()))
			}
		}()
		//err := go functionInvoker.Run(req, r.ContentLength, r, w, stopCh)

		L:
			for{
				select{
				case _ = <-stopCh:
					break L
				default:
					new_memo = readMemo()
					if new_memo > max_memo {
						max_memo = new_memo
					}
					time.Sleep(10 * time.Millisecond)
				}
			}

		//if err != nil {
		//	w.WriteHeader(500)
		//	w.Write([]byte(err.Error()))
		//}
		log.Printf("[MEMO] %f", max_memo)
		log.Printf("[CPU] %f", readCPU() - cpu_start)
	}
}

func makeStaticRequestHandler(watchdogConfig config.WatchdogConfig) http.HandlerFunc {
	if watchdogConfig.StaticPath == "" {
		log.Fatal(`For mode=static you must specify the "static_path" to serve`)
	}

	log.Printf("Serving files at %s", watchdogConfig.StaticPath)
	return http.FileServer(http.Dir(watchdogConfig.StaticPath)).ServeHTTP
}

func lockFilePresent() bool {
	path := filepath.Join(os.TempDir(), ".lock")

	if _, err := os.Stat(path); os.IsNotExist(err) {
		return false
	}
	return true
}

func makeHealthHandler() func(http.ResponseWriter, *http.Request) {
	return func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			if atomic.LoadInt32(&acceptingConnections) == 0 || lockFilePresent() == false {
				w.WriteHeader(http.StatusServiceUnavailable)
				return
			}

			w.WriteHeader(http.StatusOK)
			w.Write([]byte("OK"))

			break
		default:
			w.WriteHeader(http.StatusMethodNotAllowed)
		}
	}
}

func printVersion() {
	sha := "unknown"
	if len(GitCommit) > 0 {
		sha = GitCommit
	}

	log.Printf("Version: %v\tSHA: %v\n", BuildVersion(), sha)
}
