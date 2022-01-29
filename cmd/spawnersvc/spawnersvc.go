package main

import (
	"flag"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"text/tabwriter"

	"github.com/go-kit/kit/metrics"
	"github.com/go-kit/kit/metrics/prometheus"
	kitgrpc "github.com/go-kit/kit/transport/grpc"
	"github.com/oklog/oklog/pkg/group"
	stdprometheus "github.com/prometheus/client_golang/prometheus"
	"gitlab.com/netbook-devs/spawner-service/pb"
	"gitlab.com/netbook-devs/spawner-service/pkg/config"
	"gitlab.com/netbook-devs/spawner-service/pkg/spawnerservice"
	"gitlab.com/netbook-devs/spawner-service/pkg/spwnendpoint"
	"gitlab.com/netbook-devs/spawner-service/pkg/spwntransport"
	"go.uber.org/zap"
	"google.golang.org/grpc"
)

func main() {
	// Define our flags. Your service probably won't need to bind listeners for
	// *all* supported transports, or support both Zipkin and LightStep, and so
	// on, but we do it here for demonstration purposes.

	// Create a single zap sugar logger, which we'll use and give to other components.
	var logger, _ = zap.NewDevelopment()
	var sugar = logger.Sugar()
	defer sugar.Sync()

	fs := flag.NewFlagSet("spawnersvc", flag.ExitOnError)
	var (
		debugAddr = fs.String("debug-addr", ":8080", "Debug and metrics listen address")
		// httpAddr       = fs.String("http-addr", ":8081", "HTTP listen address")
		grpcAddr = fs.String("grpc-addr", ":8083", "gRPC listen address")
	)
	fs.Usage = usageFor(fs, os.Args[0]+" [flags]")
	fs.Parse(os.Args[1:])

	config, err := config.Load(".")
	if err != nil {
		sugar.Errorw("error loading config", "error", err.Error())
	}

	// Create the (sparse) metrics we'll use in the service. They, too, are
	// dependencies that we pass to components that use them.
	var ints metrics.Counter
	{
		// Business-level metrics.
		ints = prometheus.NewCounterFrom(stdprometheus.CounterOpts{
			Namespace: "spawnerservice",
			Subsystem: "spawnerservice",
			Name:      "integers_summed",
			Help:      "Total number of method calls",
		}, []string{})
	}

	var duration metrics.Histogram
	{
		// Endpoint-level metrics.
		duration = prometheus.NewSummaryFrom(stdprometheus.SummaryOpts{
			Namespace: "spawnerservice",
			Subsystem: "spawnerservice",
			Name:      "request_duration_seconds",
			Help:      "Request duration in seconds.",
		}, []string{"method", "success"})
	}

	// Build the layers of the service "onion" from the inside out. First, the
	// business logic service; then, the set of endpoints that wrap the service;
	// and finally, a series of concrete transport adapters. The adapters, like
	// the HTTP handler or the gRPC server, are the bridge between Go kit and
	// the interfaces that the transports expect. Note that we're not binding
	// them to ports or anything yet; we'll do that next.
	var (
		service   = spawnerservice.New(sugar, &config, ints)
		endpoints = spwnendpoint.New(service, sugar, duration)
		// httpHandler    = spwntransport.NewHTTPHandler(endpoints, tracer, zipkinTracer, logger)
		grpcServer = spwntransport.NewGRPCServer(endpoints, sugar)
	)

	// Now we're to the part of the func main where we want to start actually
	// running things, like servers bound to listeners to receive connections.
	//
	// The method is the same for each component: add a new actor to the group
	// struct, which is a combination of 2 anonymous functions: the first
	// function actually runs the component, and the second function should
	// interrupt the first function and cause it to return. It's in these
	// functions that we actually bind the Go kit server/handler structs to the
	// concrete transports and run them.
	//
	// Putting each component into its own block is mostly for aesthetics: it
	// clearly demarcates the scope in which each listener/socket may be used.
	var g group.Group
	{
		// The debug listener mounts the http.DefaultServeMux, and serves up
		// stuff like the Prometheus metrics route, the Go debug and profiling
		// routes, and so on.
		debugListener, err := net.Listen("tcp", *debugAddr)
		if err != nil {
			sugar.Errorw("error in debugListener", "transport", "debug/HTTP", "during", "Listen", "error", err)
			os.Exit(1)
		}
		g.Add(func() error {
			sugar.Infow("error in debugListener", "transport", "debug/HTTP", "debugAddr", *debugAddr)
			return http.Serve(debugListener, http.DefaultServeMux)
		}, func(error) {
			debugListener.Close()
		})
	}
	// {
	// 	// The HTTP listener mounts the Go kit HTTP handler we created.
	// 	httpListener, err := net.Listen("tcp", *httpAddr)
	// 	if err != nil {
	// 		logger.Log("transport", "HTTP", "during", "Listen", "err", err)
	// 		os.Exit(1)
	// 	}
	// 	g.Add(func() error {
	// 		logger.Log("transport", "HTTP", "addr", *httpAddr)
	// 		return http.Serve(httpListener, httpHandler)
	// 	}, func(error) {
	// 		httpListener.Close()
	// 	})
	// }
	{
		// The gRPC listener mounts the Go kit gRPC server we created.
		grpcListener, err := net.Listen("tcp", *grpcAddr)
		if err != nil {
			sugar.Errorw("error in grpcListener", "transport", "gRPC", "during", "Listen", "err", err)
			os.Exit(1)
		}
		g.Add(func() error {
			sugar.Infow("in main", "transport", "gRPC", "grpcAddr", *grpcAddr)
			// we add the Go Kit gRPC Interceptor to our gRPC service as it is used by
			// the here demonstrated zipkin tracing middleware.
			baseServer := grpc.NewServer(grpc.UnaryInterceptor(kitgrpc.Interceptor))
			pb.RegisterSpawnerServiceServer(baseServer, grpcServer)
			return baseServer.Serve(grpcListener)
		}, func(error) {
			grpcListener.Close()
		})
	}
	{
		// This function just sits and waits for ctrl-C.
		cancelInterrupt := make(chan struct{})
		g.Add(func() error {
			c := make(chan os.Signal, 1)
			signal.Notify(c, syscall.SIGINT, syscall.SIGTERM)
			select {
			case sig := <-c:
				return fmt.Errorf("received signal %s", sig)
			case <-cancelInterrupt:
				return nil
			}
		}, func(error) {
			close(cancelInterrupt)
		})
	}
	sugar.Infow("main", "exit", g.Run())
}

func usageFor(fs *flag.FlagSet, short string) func() {
	return func() {
		fmt.Fprintf(os.Stderr, "USAGE\n")
		fmt.Fprintf(os.Stderr, "  %s\n", short)
		fmt.Fprintf(os.Stderr, "\n")
		fmt.Fprintf(os.Stderr, "FLAGS\n")
		w := tabwriter.NewWriter(os.Stderr, 0, 2, 2, ' ', 0)
		fs.VisitAll(func(f *flag.Flag) {
			fmt.Fprintf(w, "\t-%s %s\t%s\n", f.Name, f.DefValue, f.Usage)
		})
		w.Flush()
		fmt.Fprintf(os.Stderr, "\n")
	}
}
