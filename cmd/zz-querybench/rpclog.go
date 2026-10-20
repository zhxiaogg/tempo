// Per-request server accounting shared by every RPC. One stderr line per request:
//
//	zzqb rpc=<SearchBlock|QueryRange|FindTraceByID> server_ns=<wall ns in handler> cpu_ns=<process CPU ns delta> requests=<backend reads> bytes=<bytes read> <extra k=v ...>
//
// cpu_ns is the user+sys delta of getrusage(RUSAGE_SELF) between handler entry and the point
// the response is ready (before gRPC marshals it), so it includes every goroutine of the
// process in that window — GC workers and parquet read-ahead included. Requests are driven
// strictly one at a time, so nothing else is in the window. The Rust servers print the same
// line; docker/run_formats.sh joins these lines to the client's requests (by req= when the
// server echoes the `zzqb-req` gRPC metadata, else by position).
package main

import (
	"context"
	"fmt"
	"os"
	"strings"
	"syscall"
	"time"

	"google.golang.org/grpc/metadata"
)

// reqTagKey is the gRPC metadata key the suite client sets on every request
// ("<config>/<query id>/<iteration>", iteration "w" for the warm-up).
const reqTagKey = "zzqb-req"

func processCPUNs() int64 {
	var ru syscall.Rusage
	if err := syscall.Getrusage(syscall.RUSAGE_SELF, &ru); err != nil {
		return -1
	}
	tv := func(t syscall.Timeval) int64 { return int64(t.Sec)*1e9 + int64(t.Usec)*1e3 }
	return tv(ru.Utime) + tv(ru.Stime)
}

type rpcTimer struct {
	rpc  string
	req  string
	t0   time.Time
	cpu0 int64
	io0  [2]int64
}

func startRPC(ctx context.Context, rpc string) *rpcTimer {
	t := &rpcTimer{rpc: rpc, req: "-"}
	if md, ok := metadata.FromIncomingContext(ctx); ok {
		if v := md.Get(reqTagKey); len(v) > 0 && v[0] != "" {
			t.req = strings.ReplaceAll(v[0], " ", "_")
		}
	}
	t.cpu0 = processCPUNs()
	t.io0 = [2]int64{ioRequests.Load(), ioBytes.Load()}
	t.t0 = time.Now()
	return t
}

// done prints the zzqb line. extra is appended verbatim (space-separated k=v pairs).
func (t *rpcTimer) done(extra string) {
	wall := time.Since(t.t0).Nanoseconds()
	cpu := processCPUNs() - t.cpu0
	reqs, bytes := ioRequests.Load()-t.io0[0], ioBytes.Load()-t.io0[1]
	fmt.Fprintf(os.Stderr, "zzqb rpc=%s server_ns=%d cpu_ns=%d req=%s requests=%d bytes=%d %s\n", t.rpc, wall, cpu, t.req, reqs, bytes, extra)
}
