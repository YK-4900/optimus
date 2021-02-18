package main

import (
	"context"
	"flag"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"cloud.google.com/go/storage"
	"github.com/grpc-ecosystem/go-grpc-middleware"
	"github.com/grpc-ecosystem/go-grpc-middleware/logging/logrus"
	grpctags "github.com/grpc-ecosystem/go-grpc-middleware/tags"
	"github.com/grpc-ecosystem/grpc-gateway/v2/runtime"
	"github.com/jinzhu/gorm"
	"github.com/pkg/errors"
	"github.com/sirupsen/logrus"
	"golang.org/x/net/http2"
	"golang.org/x/net/http2/h2c"
	"google.golang.org/grpc"
	"google.golang.org/grpc/reflection"

	v1 "github.com/odpf/optimus/api/handler/v1"
	v1handler "github.com/odpf/optimus/api/handler/v1"
	pb "github.com/odpf/optimus/api/proto/v1"
	"github.com/odpf/optimus/core/logger"
	"github.com/odpf/optimus/core/progress"
	_ "github.com/odpf/optimus/ext/hook"
	"github.com/odpf/optimus/ext/scheduler/airflow"
	_ "github.com/odpf/optimus/ext/task"
	"github.com/odpf/optimus/instance"
	"github.com/odpf/optimus/job"
	"github.com/odpf/optimus/models"
	"github.com/odpf/optimus/resources"
	"github.com/odpf/optimus/store"
	"github.com/odpf/optimus/store/gcs"
	"github.com/odpf/optimus/store/postgres"
)

var (
	// Version of the service
	// overridden by the build system. see "Makefile"
	Version = "0.1"

	// AppName is used to prefix Version
	AppName = "optimus"

	//listen for sigterm
	termChan = make(chan os.Signal, 1)

	shutdownWait = 30 * time.Second
)

// Config for the service
var Config = struct {
	ServerPort    string
	ServerHost    string
	LogLevel      string
	DBHost        string
	DBUser        string
	DBPassword    string
	DBName        string
	DBSSLMode     string
	MaxIdleDBConn string
	MaxOpenDBConn string
	IngressHost   string
}{
	ServerPort:    "9100",
	ServerHost:    "0.0.0.0",
	LogLevel:      "DEBUG",
	MaxIdleDBConn: "5",
	MaxOpenDBConn: "10",
	DBSSLMode:     "disable",
	DBPassword:    "-",
}

func lookupEnvOrString(key, fallback string) string {
	if value, ok := os.LookupEnv(key); ok {
		return value
	}
	return fallback
}

// cfg defines an input parameter to the service
type cfg struct {
	Env, Cmd, Desc string
}

// cfgRules define how input parameters map to local
// configuration variables
var cfgRules = map[*string]cfg{
	&Config.ServerPort: {
		Env:  "SERVER_PORT",
		Cmd:  "server-port",
		Desc: "port to listen on",
	},
	&Config.ServerHost: {
		Env:  "SERVER_HOST",
		Cmd:  "server-host",
		Desc: "the network interface to listen on",
	},
	&Config.LogLevel: {
		Env:  "LOG_LEVEL",
		Cmd:  "log-level",
		Desc: "log level - DEBUG, INFO, WARNING, ERROR, FATAL",
	},
	&Config.DBHost: {
		Env:  "DB_HOST",
		Cmd:  "db-host",
		Desc: "database host to connect to database used by jazz",
	},
	&Config.DBUser: {
		Env:  "DB_USER",
		Cmd:  "db-user",
		Desc: "database user to connect to database used by jazz",
	},
	&Config.DBPassword: {
		Env:  "DB_PASSWORD",
		Cmd:  "db-password",
		Desc: "database password to connect to database used by jazz",
	},
	&Config.DBName: {
		Env:  "DB_NAME",
		Cmd:  "db-name",
		Desc: "database name to connect to database used by jazz",
	},
	&Config.DBSSLMode: {
		Env:  "DB_SSL_MODE",
		Cmd:  "db-ssl-mode",
		Desc: "database sslmode to connect to database used by jazz (require, disable)",
	},
	&Config.MaxIdleDBConn: {
		Env:  "MAX_IDLE_DB_CONN",
		Cmd:  "max-idle-db-conn",
		Desc: "maximum allowed idle DB connections",
	},
	&Config.IngressHost: {
		Env:  "INGRESS_HOST",
		Cmd:  "ingress-host",
		Desc: "service ingress host for jobs to communicate back to optimus",
	},
}

func validateConfig() error {
	var errs []string
	for v, cfg := range cfgRules {
		if strings.TrimSpace(*v) == "" {
			errs = append(
				errs,
				fmt.Sprintf(
					"missing required parameter: -%s (can also be set using %s environment variable)",
					cfg.Cmd,
					cfg.Env,
				),
			)
		}
		if *v == "-" { // "- is used for empty arguments"
			*v = ""
		}
	}
	if len(errs) > 0 {
		return fmt.Errorf("%s", strings.Join(errs, "\n"))
	}
	return nil
}

// jobSpecRepoFactory stores raw specifications
type jobSpecRepoFactory struct {
	db *gorm.DB
}

func (fac *jobSpecRepoFactory) New(proj models.ProjectSpec) store.JobSpecRepository {
	return postgres.NewJobRepository(fac.db, proj, postgres.NewAdapter(models.TaskRegistry, models.HookRegistry))
}

// jobRepoFactory stores compiled specifications that will be consumed by a
// scheduler
type jobRepoFactory struct {
	gcsClient *storage.Client
	schd      models.SchedulerUnit
}

func (fac *jobRepoFactory) New(proj models.ProjectSpec) (store.JobRepository, error) {
	storagePath, ok := proj.Config[models.ProjectStoragePathKey]
	if !ok {
		return nil, errors.Errorf("%s not configured for project %s", models.ProjectStoragePathKey, proj.Name)
	}
	p, err := url.Parse(storagePath)
	if err != nil {
		return nil, err
	}
	switch p.Scheme {
	case "gs":
		return gcs.NewJobRepository(p.Hostname(), filepath.Join(p.Path, fac.schd.GetJobsDir()), fac.schd.GetJobsExtension(), fac.gcsClient), nil
	}
	return nil, errors.Errorf("unsupported storage config %s in %s of project %s", storagePath, models.ProjectStoragePathKey, proj.Name)
}

type projectRepoFactory struct {
	db *gorm.DB
}

func (fac *projectRepoFactory) New() store.ProjectRepository {
	return postgres.NewProjectRepository(fac.db)
}

type instanceRepoFactory struct {
	db *gorm.DB
}

func (fac *instanceRepoFactory) New(spec models.JobSpec) store.InstanceSpecRepository {
	return postgres.NewInstanceRepository(fac.db, spec, postgres.NewAdapter(models.TaskRegistry, models.HookRegistry))
}

type pipelineLogObserver struct {
	log logrus.FieldLogger
}

func (obs *pipelineLogObserver) Notify(evt progress.Event) {
	obs.log.Info(evt)
}

func init() {
	for v, cfg := range cfgRules {
		flag.StringVar(v, cfg.Cmd, lookupEnvOrString(cfg.Env, *v), cfg.Desc)
	}
	flag.Parse()
}

func main() {

	log := logrus.New()
	log.SetOutput(os.Stdout)
	logger.Init(Config.LogLevel)

	mainLog := log.WithField("reporter", "main")
	mainLog.Infof("starting optimus %s", Version)

	err := validateConfig()
	if err != nil {
		mainLog.Fatalf("configuration error:\n%v", err)
	}

	progressObs := &pipelineLogObserver{
		log: log.WithField("reporter", "pipeline"),
	}

	// setup db
	maxIdleConnection, _ := strconv.Atoi(Config.MaxIdleDBConn)
	maxOpenConnection, _ := strconv.Atoi(Config.MaxOpenDBConn)
	databaseURL := fmt.Sprintf("postgres://%s:%s@%s:5432/%s?sslmode=%s", Config.DBUser, url.QueryEscape(Config.DBPassword), Config.DBHost, Config.DBName, Config.DBSSLMode)
	if err := postgres.Migrate(databaseURL); err != nil {
		panic(err)
	}
	dbConn, err := postgres.Connect(databaseURL, maxIdleConnection, maxOpenConnection)
	if err != nil {
		panic(err)
	}

	// gcs storage client for storing project compiled specifications
	googleStorage, err := storage.NewClient(context.Background())
	if err != nil {
		logger.F("error creating google storage client: %v", err)
	}

	// init default scheduler, should be configurable by user configs later
	models.Scheduler = airflow.NewScheduler(
		resources.FileSystem,
		&gcs.GcsObjectWriter{
			Client: googleStorage,
		},
	)

	// registered project store repository factory, its a wrapper over a storage
	// interface
	projectRepoFac := &projectRepoFactory{
		db: dbConn,
	}
	registeredProjects, err := projectRepoFac.New().GetAll()
	if err != nil {
		panic(err)
	}
	// bootstrap scheduler for registered projects
	for _, proj := range registeredProjects {
		if err := models.Scheduler.Bootstrap(context.Background(), proj); err != nil {
			// TODO: ideally should panic out
			logger.E(err)
		}
	}

	// Logrus entry is used, allowing pre-definition of certain fields by the user.
	logrusEntry := logrus.NewEntry(log)
	// Shared options for the logger, with a custom gRPC code to log level function.
	opts := []grpc_logrus.Option{
		grpc_logrus.WithLevels(grpc_logrus.DefaultCodeToLevel),
	}
	// Make sure that log statements internal to gRPC library are logged using the logrus Logger as well.
	grpc_logrus.ReplaceGrpcLogger(logrusEntry)

	serverPort, err := strconv.Atoi(Config.ServerPort)
	if err != nil {
		panic("invalid server port")
	}
	grpcAddr := fmt.Sprintf("%s:%d", Config.ServerHost, serverPort)
	grpcOpts := []grpc.ServerOption{
		grpc_middleware.WithUnaryServerChain(
			grpctags.UnaryServerInterceptor(grpctags.WithFieldExtractor(grpctags.CodeGenRequestFieldExtractor)),
			grpc_logrus.UnaryServerInterceptor(logrusEntry, opts...),
		),
	}
	grpcServer := grpc.NewServer(grpcOpts...)
	reflection.Register(grpcServer)

	// runtime service instance over gprc
	pb.RegisterRuntimeServiceServer(grpcServer, v1handler.NewRuntimeServiceServer(
		Version,
		job.NewService(
			&jobSpecRepoFactory{
				db: dbConn,
			},
			&jobRepoFactory{
				gcsClient: googleStorage,
				schd:      models.Scheduler,
			},
			job.NewCompiler(resources.FileSystem, models.Scheduler.GetTemplatePath(), Config.IngressHost),
			job.NewDependencyResolver(),
			job.NewPriorityResolver(),
		),
		projectRepoFac,
		v1.NewAdapter(models.TaskRegistry, models.HookRegistry),
		progressObs,
		instance.NewService(
			&instanceRepoFactory{
				db: dbConn,
			},
			time.Now().UTC,
		),
	))

	// prepare http proxy
	gwmux := runtime.NewServeMux(
		runtime.WithErrorHandler(runtime.DefaultHTTPErrorHandler),
	)
	// gRPC dialup options to proxy http connections
	grpcConn, err := grpc.Dial(grpcAddr, []grpc.DialOption{
		grpc.WithTimeout(10 * time.Second),
		grpc.WithInsecure(),
	}...)
	if err != nil {
		panic(fmt.Errorf("Fail to dial: %v", err))
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := pb.RegisterRuntimeServiceHandler(ctx, gwmux, grpcConn); err != nil {
		panic(err)
	}

	// base router
	baseMux := http.NewServeMux()
	baseMux.HandleFunc("/ping", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, "pong")
	}))
	baseMux.Handle("/", gwmux)

	srv := &http.Server{
		Handler:      grpcHandlerFunc(grpcServer, baseMux),
		Addr:         grpcAddr,
		ReadTimeout:  5 * time.Second,
		WriteTimeout: 10 * time.Second,
		IdleTimeout:  120 * time.Second,
	}

	// run our server in a goroutine so that it doesn't block.
	go func() {
		mainLog.Infoln("starting listening at ", grpcAddr)
		if err := srv.ListenAndServe(); err != nil {
			if err != http.ErrServerClosed {
				mainLog.Fatalf("server error: %v\n", err)
			}
		}
	}()

	// We'll accept graceful shutdowns when quit via SIGINT (Ctrl+C)
	signal.Notify(termChan, os.Interrupt)
	signal.Notify(termChan, os.Kill)
	signal.Notify(termChan, syscall.SIGTERM)

	// Block until we receive our signal.
	<-termChan
	mainLog.Info("termination request received")

	// Create a deadline to wait for server
	ctxProxy, cancelProxy := context.WithTimeout(context.Background(), shutdownWait)
	defer cancelProxy()

	// Doesn't block if no connections, but will otherwise wait
	// until the timeout deadline.
	if err := srv.Shutdown(ctxProxy); err != nil {
		mainLog.Warn(err)
	}
	grpcServer.GracefulStop()

	mainLog.Info("bye")
}

// grpcHandlerFunc routes http1 calls to baseMux and http2 with grpc header to grpcServer.
// Using a single port for proxying both http1 & 2 protocols will degrade http performance
// but for our usecase the convenience per performance tradeoff is better suited
// if in future, this does become a bottleneck(which I highly doubt), we can break the service
// into two ports, default port for grpc and default+1 for grpc-gateway proxy.
// We can also use something like a connection multiplexer
// https://github.com/soheilhy/cmux to achieve the same.
func grpcHandlerFunc(grpcServer *grpc.Server, otherHandler http.Handler) http.Handler {
	return h2c.NewHandler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.ProtoMajor == 2 && strings.Contains(r.Header.Get("Content-Type"), "application/grpc") {
			grpcServer.ServeHTTP(w, r)
		} else {
			otherHandler.ServeHTTP(w, r)
		}
	}), &http2.Server{})
}