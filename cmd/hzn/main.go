package main

import (
	"bytes"
	"context"
	"crypto/tls"
	"fmt"
	"io/ioutil"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/credentials"
	"github.com/aws/aws-sdk-go/aws/session"
	"github.com/aws/aws-sdk-go/service/s3"
	"github.com/golang-migrate/migrate/v4"
	_ "github.com/golang-migrate/migrate/v4/database/postgres"
	_ "github.com/golang-migrate/migrate/v4/source/file"
	"github.com/hashicorp/go-hclog"
	"github.com/hashicorp/horizon/pkg/config"
	"github.com/hashicorp/horizon/pkg/control"
	"github.com/hashicorp/horizon/pkg/discovery"
	"github.com/hashicorp/horizon/pkg/grpc/lz4"
	grpctoken "github.com/hashicorp/horizon/pkg/grpc/token"
	"github.com/hashicorp/horizon/pkg/hub"
	"github.com/hashicorp/horizon/pkg/pb"
	"github.com/hashicorp/horizon/pkg/periodic"
	"github.com/hashicorp/horizon/pkg/tlsmanage"
	"github.com/hashicorp/horizon/pkg/utils"
	"github.com/hashicorp/horizon/pkg/workq"
	"github.com/hashicorp/vault/api"
	"github.com/jinzhu/gorm"
	"github.com/mitchellh/cli"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"google.golang.org/grpc"
	"google.golang.org/grpc/metadata"
)

var (
	sha1ver   string // sha1 revision used to build the program
	buildTime string // when the executable was built
)

func main() {
	var ver string
	if sha1ver == "" {
		ver = "unknown"
	} else {
		ver = sha1ver[:10] + "-" + buildTime
	}

	c := cli.NewCLI("hzn", ver)
	c.Args = os.Args[1:]
	c.Commands = map[string]cli.CommandFactory{
		"control": controlFactory,
		"dev": func() (cli.Command, error) {
			return &devServer{}, nil
		},
		"hub": hubFactory,
		"migrate": func() (cli.Command, error) {
			return &migrateRunner{}, nil
		},
	}

	fmt.Printf("hzn: %s\n", ver)

	exitStatus, err := c.Run()
	if err != nil {
		log.Println(err)
	}

	os.Exit(exitStatus)
}

func controlFactory() (cli.Command, error) {
	return &controlServer{}, nil
}

type migrateRunner struct{}

func (m *migrateRunner) Help() string {
	return "run any migrations"
}

func (m *migrateRunner) Synopsis() string {
	return "run any migrations"
}

func (mr *migrateRunner) Run(args []string) int {
	url := os.Getenv("DATABASE_URL")
	if url == "" {
		log.Fatal("no DATABASE_URL provided")
	}

	migPath := os.Getenv("MIGRATIONS_PATH")
	if migPath == "" {
		migPath = "/migrations"
	}

	m, err := migrate.New("file://"+migPath, url)
	if err != nil {
		log.Fatal(err)
	}

	err = m.Up()
	if err != nil {
		log.Fatal(err)
	}

	return 0
}

func hubFactory() (cli.Command, error) {
	return &hubRunner{}, nil
}

func StartHealthz(L hclog.Logger) {
	healthzPort := os.Getenv("HEALTHZ_PORT")
	if healthzPort == "" {
		healthzPort = "17001"
	}

	L.Info("starting healthz/metrics server", "port", healthzPort)

	handlerOptions := promhttp.HandlerOpts{
		ErrorLog:           L.Named("prometheus_handler").StandardLogger(nil),
		ErrorHandling:      promhttp.ContinueOnError,
		DisableCompression: true,
	}

	promHandler := promhttp.HandlerFor(prometheus.DefaultGatherer, handlerOptions)

	mux := http.NewServeMux()
	mux.Handle("/metrics", promHandler)
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
	})

	http.ListenAndServe(":"+healthzPort, mux)
}

type controlServer struct{}

func (c *controlServer) Help() string {
	return "Start a control server"
}

func (c *controlServer) Synopsis() string {
	return "Start a control server"
}

func (c *controlServer) Run(args []string) int {
	level := hclog.Info
	if os.Getenv("DEBUG") != "" {
		level = hclog.Trace
	}

	L := hclog.New(&hclog.LoggerOptions{
		Name:  "control",
		Level: level,
		Exclude: hclog.ExcludeFuncs{
			hclog.ExcludeByPrefix("http: TLS handshake error from").Exclude,
		}.Exclude,
	})

	L.Info("log level configured", "level", level)
	L.Trace("starting server")

	vcfg := api.DefaultConfig()

	vc, err := api.NewClient(vcfg)
	if err != nil {
		log.Fatal(err)
	}

	// If we have token AND this is kubernetes, then let's try to get a token
	if vc.Token() == "" {
		f, err := os.Open("/var/run/secrets/kubernetes.io/serviceaccount/token")
		if err == nil {
			L.Info("attempting to login to vault via kubernetes auth")

			data, err := ioutil.ReadAll(f)
			if err != nil {
				log.Fatal(err)
			}

			f.Close()

			sec, err := vc.Logical().Write("auth/kubernetes/login", map[string]interface{}{
				"role": "horizon",
				"jwt":  string(bytes.TrimSpace(data)),
			})
			if err != nil {
				log.Fatal(err)
			}

			if sec == nil {
				log.Fatal("unable to login to get token")
			}

			vc.SetToken(sec.Auth.ClientToken)

			L.Info("retrieved token from vault", "accessor", sec.Auth.Accessor)

			go func() {
				tic := time.NewTicker(time.Hour)
				for {
					<-tic.C
					vc.Auth().Token().RenewSelf(86400)
				}
			}()
		}
	}

	url := os.Getenv("DATABASE_URL")
	if url == "" {
		log.Fatal("no DATABASE_URL provided")
	}

	db, err := gorm.Open("postgres", url)
	if err != nil {
		log.Fatal(err)
	}

	sess := session.New()

	bucket := os.Getenv("S3_BUCKET")
	if bucket == "" {
		log.Fatal("S3_BUCKET not set")
	}

	domain := os.Getenv("HUB_DOMAIN")
	if domain == "" {
		log.Fatal("missing HUB_DOMAIN")
	}

	staging := os.Getenv("LETSENCRYPT_STAGING") != ""

	tlsmgr, err := tlsmanage.NewManager(tlsmanage.ManagerConfig{
		L:           L,
		Domain:      domain,
		VaultClient: vc,
		Staging:     staging,
	})
	if err != nil {
		log.Fatal(err)
	}

	zoneId := os.Getenv("ZONE_ID")
	if zoneId == "" {
		log.Fatal("missing ZONE_ID")
	}

	err = tlsmgr.SetupRoute53(sess, zoneId)
	if err != nil {
		log.Fatal(err)
	}

	regTok := os.Getenv("REGISTER_TOKEN")
	if regTok == "" {
		log.Fatal("missing REGISTER_TOKEN")
	}

	opsTok := os.Getenv("OPS_TOKEN")
	if opsTok == "" {
		log.Fatal("missing OPS_TOKEN")
	}

	dynamoTable := os.Getenv("DYNAMO_TABLE")
	if dynamoTable == "" {
		log.Fatal("missing DYNAMO_TABLE")
	}

	asnDB := os.Getenv("ASN_DB_PATH")

	hubAccess := os.Getenv("HUB_ACCESS_KEY")
	hubSecret := os.Getenv("HUB_SECRET_KEY")
	hubTag := os.Getenv("HUB_IMAGE_TAG")

	port := os.Getenv("PORT")

	go StartHealthz(L)

	ctx := hclog.WithContext(context.Background(), L)

	cert, key, err := tlsmgr.HubMaterial(ctx)
	if err != nil {
		log.Fatal(err)
	}

	s, err := control.NewServer(control.ServerConfig{
		Logger: L,
		DB:     db,

		RegisterToken: regTok,
		OpsToken:      opsTok,

		VaultClient: vc,
		VaultPath:   "hzn-k1",
		KeyId:       "k1",

		AwsSession: sess,
		Bucket:     bucket,
		LockTable:  dynamoTable,

		ASNDB: asnDB,

		HubAccessKey: hubAccess,
		HubSecretKey: hubSecret,
		HubImageTag:  hubTag,
	})
	if err != nil {
		log.Fatal(err)
	}

	// Setup cleanup activities
	lc := &control.LogCleaner{DB: config.DB()}
	workq.RegisterHandler("cleanup-activity-log", lc.CleanupActivityLog)
	workq.RegisterPeriodicJob("cleanup-activity-log", "default", "cleanup-activity-log", 0, time.Hour)

	hubDomain := domain
	if strings.HasPrefix(hubDomain, "*.") {
		hubDomain = hubDomain[2:]
	}

	s.SetHubTLS(cert, key, hubDomain)

	// So that when they are refreshed by the background job, we eventually pick
	// them up. Hubs are also refreshing their config on an hourly basis so they'll
	// end up picking up the new TLS material that way too.
	go periodic.Run(ctx, time.Hour, func() {
		cert, key, err := tlsmgr.RefreshFromVault()
		if err != nil {
			L.Error("error refreshing hub certs from vault")
		} else {
			s.SetHubTLS(cert, key, hubDomain)
		}
	})

	gs := grpc.NewServer()
	pb.RegisterControlServicesServer(gs, s)
	pb.RegisterControlManagementServer(gs, s)
	pb.RegisterFlowTopReporterServer(gs, s)

	tlsCert, err := tlsmgr.Certificate()
	if err != nil {
		log.Fatal(err)
	}

	var lcfg tls.Config
	lcfg.Certificates = []tls.Certificate{tlsCert}

	hs := &http.Server{
		TLSConfig: &lcfg,
		Addr:      ":" + port,
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.ProtoMajor == 2 &&
				strings.HasPrefix(r.Header.Get("Content-Type"), "application/grpc") {
				gs.ServeHTTP(w, r)
			} else {
				s.ServeHTTP(w, r)
			}
		}),
		ErrorLog: L.StandardLogger(&hclog.StandardLoggerOptions{
			InferLevels: true,
		}),
	}

	tlsmgr.RegisterRenewHandler(L, workq.GlobalRegistry)

	L.Info("starting background worker")

	workq.GlobalRegistry.PrintHandlers(L)

	worker := workq.NewWorker(L, db, []string{"default"})
	go worker.Run(ctx, workq.RunConfig{
		ConnInfo: url,
	})

	err = hs.ListenAndServeTLS("", "")
	if err != nil {
		log.Fatal(err)
	}

	return 0
}

type hubRunner struct{}

func (h *hubRunner) Help() string {
	return "Start a hub"
}

func (h *hubRunner) Synopsis() string {
	return "Start a hub"
}

func (h *hubRunner) Run(args []string) int {
	L := hclog.L().Named("hub")

	if os.Getenv("DEBUG") != "" {
		L.SetLevel(hclog.Trace)
	}

	token := os.Getenv("TOKEN")
	if token == "" {
		log.Fatal("missing TOKEN")
	}

	addr := os.Getenv("CONTROL_ADDR")
	if addr == "" {
		log.Fatal("missing ADDR")
	}

	port := os.Getenv("PORT")
	if port == "" {
		L.Info("defaulting port to 443")
		port = "443"
	}

	httpPort := os.Getenv("HTTP_PORT")

	ctx := hclog.WithContext(context.Background(), L)

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, os.Interrupt, syscall.SIGQUIT)

	go func() {
		for {
			s := <-sigs
			L.Info("signal received, closing down", "signal", s)
			cancel()
		}
	}()

	sid := os.Getenv("STABLE_ID")
	if sid == "" {
		log.Fatal("missing STABLE_ID")
	}

	webNamespace := os.Getenv("WEB_NAMESPACE")
	if webNamespace == "" {
		L.Info("defaulting to namespace for frontend", "namespace", "/waypoint")
		webNamespace = "/waypoint"
	}

	id, err := pb.ParseULID(sid)
	if err != nil {
		log.Fatal(err)
	}

	tmpdir, err := ioutil.TempDir("", "hzn")
	if err != nil {
		log.Fatal(err)
	}

	defer os.RemoveAll(tmpdir)

	deployment := os.Getenv("K8_DEPLOYMENT")

	client, err := control.NewClient(ctx, control.ClientConfig{
		Id:           id,
		Token:        token,
		Version:      "test",
		Addr:         addr,
		WorkDir:      tmpdir,
		K8Deployment: deployment,
	})

	if deployment != "" {
		err = client.ConnectToKubernetes()
		if err != nil {
			L.Error("error connecting to kubernetes", "error", err)
		}

		// Best to keep running here rather than fail so that hubs
		// don't go into crash loops but rather just don't the ability to update
		// themselves.
	}

	defer func() {
		// Get a new context to process the closure because the main one
		// is most likely closed. We also update ctx and cancel in the
		// primary closure so that the signal can cancel the close if
		// sent again.
		ctx, cancel = context.WithCancel(context.Background())
		client.Close(ctx)
	}()

	var labels *pb.LabelSet

	strLabels := os.Getenv("LOCATION_LABELS")
	if strLabels != "" {
		labels = pb.ParseLabelSet(os.Getenv(strLabels))
	}

	locs, err := client.LearnLocations(labels)
	if err != nil {
		log.Fatal(err)
	}

	err = client.BootstrapConfig(ctx)
	if err != nil {
		log.Fatal(err)
	}

	go func() {
		err := client.Run(ctx)
		if err != nil {
			L.Error("error running control client background tasks", "error", err)
		}
	}()

	L.Info("generating token to access accounts for web")
	serviceToken, err := client.RequestServiceToken(ctx, webNamespace)
	if err != nil {
		log.Fatal(err)
	}

	ln, err := net.Listen("tcp", ":"+port)
	if err != nil {
		log.Fatal(err)
	}

	defer ln.Close()

	hb, err := hub.NewHub(L, client, serviceToken)
	if err != nil {
		log.Fatal(err)
	}

	for _, loc := range locs {
		L.Info("learned network location", "labels", loc.Labels, "addresses", loc.Addresses)
	}

	if httpPort != "" {
		L.Info("listen on http", "port", httpPort)
		go hb.ListenHTTP(":" + httpPort)
	}

	go StartHealthz(L)

	err = hb.Run(ctx, ln)
	if err != nil {
		log.Fatal(err)
	}

	return 0
}

type devServer struct{}

func (c *devServer) Help() string {
	return "Start a dev control server"
}

func (c *devServer) Synopsis() string {
	return "Start a dev control server"
}

func (c *devServer) Run(args []string) int {
	L := hclog.New(&hclog.LoggerOptions{
		Name:  "control",
		Level: hclog.Info,
		Exclude: hclog.ExcludeFuncs{
			hclog.ExcludeByPrefix("http: TLS handshake error from").Exclude,
		}.Exclude,
	})

	if os.Getenv("DEBUG") != "" {
		L.SetLevel(hclog.Trace)
	}

	vc := utils.SetupVault()

	url := os.Getenv("DATABASE_URL")
	if url == "" {
		url = config.DevDBUrl
		L.Info("using default dev url for postgres", "url", url)
	}

	db, err := gorm.Open("postgres", url)
	if err != nil {
		log.Fatal(err)
	}

	sess := session.New(aws.NewConfig().
		WithEndpoint("http://localhost:4566").
		WithRegion("us-east-1").
		WithCredentials(credentials.NewStaticCredentials("hzn", "hzn", "hzn")).
		WithS3ForcePathStyle(true),
	)

	bucket := os.Getenv("S3_BUCKET")
	if bucket == "" {
		bucket = "hzn-dev"
		L.Info("using hzn-dev as the S3 bucket")
	}

	s3.New(sess).CreateBucket(&s3.CreateBucketInput{
		Bucket: aws.String(bucket),
	})

	domain := os.Getenv("HUB_DOMAIN")
	if domain == "" {
		domain = "localdomain"
		L.Info("using localdomain as hub domain")
	}

	regTok := os.Getenv("REGISTER_TOKEN")
	if regTok == "" {
		regTok = "aabbcc"
		L.Info("using default register token", "token", regTok)
	}

	opsTok := os.Getenv("OPS_TOKEN")
	if opsTok == "" {
		opsTok = regTok
		L.Info("using default ops token", "token", opsTok)
	}

	go StartHealthz(L)

	ctx := hclog.WithContext(context.Background(), L)

	s, err := control.NewServer(control.ServerConfig{
		DB: db,

		RegisterToken: regTok,
		OpsToken:      opsTok,

		VaultClient: vc,
		VaultPath:   "hzn-dev",
		KeyId:       "dev",

		AwsSession: sess,
		Bucket:     bucket,
		LockTable:  "hzndev",
	})
	if err != nil {
		log.Fatal(err)
	}

	hubDomain := domain
	if strings.HasPrefix(hubDomain, "*.") {
		hubDomain = hubDomain[2:]
	}

	cert, key, err := utils.SelfSignedCert()
	if err != nil {
		log.Fatal(err)
	}

	s.SetHubTLS(cert, key, hubDomain)

	gs := grpc.NewServer()
	pb.RegisterControlServicesServer(gs, s)
	pb.RegisterControlManagementServer(gs, s)
	pb.RegisterFlowTopReporterServer(gs, s)

	li, err := net.Listen("tcp", ":24401")
	if err != nil {
		log.Fatal(err)
	}

	defer li.Close()

	go gs.Serve(li)

	hs := &http.Server{
		Addr: ":24402",
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.ProtoMajor == 2 &&
				strings.HasPrefix(r.Header.Get("Content-Type"), "application/grpc") {
				gs.ServeHTTP(w, r)
			} else {
				if r.URL.Path == discovery.HTTPPath {
					w.Write([]byte(`{"hubs": [{"addresses":["127.0.0.1:24403"],"labels":{"labels":[{"name":"type","value":"dev"}]}, "name":"dev.localdomain"}]}`))
				} else {
					s.ServeHTTP(w, r)
				}
			}
		}),
		ErrorLog: L.StandardLogger(&hclog.StandardLoggerOptions{
			InferLevels: true,
		}),
	}

	L.Info("starting background worker")

	workq.GlobalRegistry.PrintHandlers(L)

	worker := workq.NewWorker(L, db, []string{"default"})
	go worker.Run(ctx, workq.RunConfig{
		ConnInfo: url,
	})

	md := make(metadata.MD)
	md.Set("authorization", regTok)

	ictx := metadata.NewIncomingContext(ctx, md)

	ctr, err := s.IssueHubToken(ictx, &pb.Noop{})
	if err != nil {
		log.Fatal(err)
	}

	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, os.Interrupt, syscall.SIGQUIT)

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	go func() {
		for {
			s := <-sigs
			L.Info("signal received, closing down", "signal", s)
			cancel()
			hs.Close()
		}
	}()

	mgmtToken, err := s.GetManagementToken(ctx, "/waypoint")
	if err != nil {
		log.Fatal(err)
	}

	md2 := make(metadata.MD)
	md2.Set("authorization", mgmtToken)

	accountId := pb.NewULID()

	agentToken, err := s.CreateToken(
		metadata.NewIncomingContext(ctx, md2),
		&pb.CreateTokenRequest{
			Account: &pb.Account{
				AccountId: accountId,
				Namespace: "/waypoint",
			},
			Capabilities: []pb.TokenCapability{
				{
					Capability: pb.SERVE,
				},
			},
		})
	if err != nil {
		log.Fatal(err)
	}

	L.Info("dev agent token", "token", agentToken.Token)

	ioutil.WriteFile("dev-mgmt-token.txt", []byte(mgmtToken), 0644)
	ioutil.WriteFile("dev-agent-id.txt", []byte(accountId.String()), 0644)
	ioutil.WriteFile("dev-agent-token.txt", []byte(agentToken.Token), 0644)

	go c.RunHub(ctx, ctr.Token, "localhost:24401", sess, bucket)
	err = hs.ListenAndServe()
	if err != nil {
		log.Fatal(err)
	}

	return 0
}

const devHub = "01ECNJBS294ESNMG913SVX893F"

func (h *devServer) RunHub(ctx context.Context, token, addr string, sess *session.Session, bucket string) int {
	L := hclog.L().Named("hub")

	if os.Getenv("DEBUG") != "" {
		L.SetLevel(hclog.Trace)
	}

	port := "24403"
	httpPort := "24404"

	ctx = hclog.WithContext(ctx, L)

	sid := devHub

	webNamespace := os.Getenv("WEB_NAMESPACE")
	if webNamespace == "" {
		L.Info("defaulting to namespace for frontend", "namespace", "/waypoint")
		webNamespace = "/waypoint"
	}

	id, err := pb.ParseULID(sid)
	if err != nil {
		log.Fatal(err)
	}

	tmpdir, err := ioutil.TempDir("", "hzn")
	if err != nil {
		log.Fatal(err)
	}

	defer os.RemoveAll(tmpdir)

	gcc, err := grpc.Dial(addr,
		grpc.WithInsecure(),
		grpc.WithPerRPCCredentials(grpctoken.Token(token)),
		grpc.WithDefaultCallOptions(grpc.UseCompressor(lz4.Name)),
	)
	if err != nil {
		log.Fatal(err)
	}

	defer gcc.Close()

	gClient := pb.NewControlServicesClient(gcc)

	client, err := control.NewClient(ctx, control.ClientConfig{
		Id:       id,
		Token:    token,
		Version:  "test",
		Client:   gClient,
		WorkDir:  tmpdir,
		Session:  sess,
		S3Bucket: bucket,
		Insecure: true,
	})

	defer client.Close(ctx)

	var labels *pb.LabelSet

	strLabels := os.Getenv("LOCATION_LABELS")
	if strLabels != "" {
		labels = pb.ParseLabelSet(os.Getenv(strLabels))
	}

	locs, err := client.LearnLocations(labels)
	if err != nil {
		log.Fatal(err)
	}

	err = client.BootstrapConfig(ctx)
	if err != nil {
		log.Fatal(err)
	}

	go func() {
		err := client.Run(ctx)
		if err != nil {
			L.Error("error running control client background tasks", "error", err)
		}
	}()

	L.Info("generating token to access accounts for web")
	serviceToken, err := client.RequestServiceToken(ctx, webNamespace)
	if err != nil {
		log.Fatal(err)
	}

	ln, err := net.Listen("tcp", ":"+port)
	if err != nil {
		log.Fatal(err)
	}

	defer ln.Close()

	hb, err := hub.NewHub(L, client, serviceToken)
	if err != nil {
		log.Fatal(err)
	}

	for _, loc := range locs {
		L.Info("learned network location", "labels", loc.Labels, "addresses", loc.Addresses)
	}

	if httpPort != "" {
		L.Info("listen on http", "port", httpPort)
		go hb.ListenHTTP(":" + httpPort)
	}

	go StartHealthz(L)

	err = hb.Run(ctx, ln)
	if err != nil {
		log.Fatal(err)
	}

	return 0
}
