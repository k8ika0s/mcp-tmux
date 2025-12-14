package main

import (
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"strings"

	"github.com/k8ika0s/mcp-tmux/go/internal/server"
	tmuxproto "github.com/k8ika0s/mcp-tmux/go/proto"
	"google.golang.org/grpc"
	"google.golang.org/grpc/health"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"
	"google.golang.org/grpc/reflection"
)

func main() {
	addr := flag.String("listen", ":9000", "gRPC listen address")
	tmuxBin := flag.String("tmux", "tmux", "tmux binary")
	pathAdd := flag.String("path-add", "/opt/homebrew/bin:/usr/local/bin:/usr/bin", "extra PATH entries (colon-separated)")
	enableReflection := flag.Bool("reflection", true, "enable gRPC reflection")
	pkgName := flag.String("pkg", "github.com/k8ika0s/mcp-tmux", "package name to report in ServerInfo")
	version := flag.String("version", "dev", "version to report in ServerInfo")
	repo := flag.String("repo", "https://github.com/k8ika0s/mcp-tmux", "repo URL to report in ServerInfo")
	authToken := flag.String("auth-token", "", "optional bearer/token required on incoming calls (authorization or x-mcp-token)")
	logFile := flag.String("log-file", "", "optional path to append audit logs")
	logColor := flag.Bool("log-color", true, "colorize audit logs")
	flag.Parse()

	lis, err := net.Listen("tcp", *addr)
	if err != nil {
		log.Fatalf("listen: %v", err)
	}

	if *logFile != "" {
		f, err := os.OpenFile(*logFile, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
		if err != nil {
			log.Fatalf("open log file: %v", err)
		}
		log.SetOutput(io.MultiWriter(os.Stdout, f))
	}

	opts := []grpc.ServerOption{}
	opts = append(opts, server.AuthOptions(*authToken)...)
	opts = append(opts, server.AuditOptions(*logColor)...)
	grpcServer := grpc.NewServer(opts...)
	meta := server.RunMeta{
		PackageName: *pkgName,
		Version:     *version,
		RepoURL:     *repo,
	}
	svc := server.NewServiceWithRunner(*tmuxBin, strings.Split(*pathAdd, ":"), server.MakeRunnerWithMeta(meta), meta)
	tmuxproto.RegisterTmuxServiceServer(grpcServer, svc)
	healthServer := health.NewServer()
	healthServer.SetServingStatus("", healthpb.HealthCheckResponse_SERVING)
	healthpb.RegisterHealthServer(grpcServer, healthServer)
	if *enableReflection {
		reflection.Register(grpcServer)
	}

	fmt.Printf("tmux gRPC server listening on %s (tmux=%s)\n", *addr, *tmuxBin)
	if err := grpcServer.Serve(lis); err != nil {
		log.Fatalf("serve: %v", err)
	}
}
