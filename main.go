// Copyright (c) 2013-2016 The btcsuite developers
// Use of this source code is governed by an ISC
// license that can be found in the LICENSE file.

package main

import (
	"context"
	"fmt"
	_ "net/http/pprof"
	"os"
	"runtime"
	"runtime/debug"
	"errors"
	//"runtime/pprof"

	"github.com/btcboost/copernicus/model/chainparams"
	"github.com/btcboost/copernicus/net/limits"
	"github.com/btcboost/copernicus/net/server"
	"github.com/btcboost/copernicus/rpc"
	"github.com/btcboost/copernicus/conf"
	"github.com/btcboost/copernicus/service"
)

const (
	// blockDbNamePrefix is the prefix for the block database name.  The
	// database type is appended to this value to form the full block
	// database name.
	blockDbNamePrefix = "blocks"
)

// btcdMain is the real main function for btcd.  It is necessary to work around
// the fact that deferred functions do not run when os.Exit() is called.  The
// optional serverChan parameter is mainly used by the service code to be
// notified with the server once it is setup so it can gracefully stop it when
// requested from the service control manager.
func bchMain(ctx context.Context) error {
	// Load configuration and parse command line.  This function also
	// initializes logging and configures it accordingly.

	interrupt := interruptListener()

	s, err := server.NewServer(&chainparams.TestNet3Params, interrupt)
	if err != nil {
		return err
	}
	//service2.NewMsgHandle(s.mh, )
	var rpcServer *rpc.Server
	if !conf.Cfg.P2PNet.DisableRPC {
		rpcServer, err = rpc.InitRPCServer()
		if err != nil {
			return errors.New("failed to init rpc")
		}
		// Start the rebroadcastHandler, which ensures user tx received by
		// the RPC server are rebroadcast until being included in a block.
		//go s.rebroadcastHandler()
		rpcServer.Start()
	}

	service.NewMsgHandle(context.TODO(), s.PhCh, s)
	if interruptRequested(interrupt) {
		return nil
	}
	s.Start()
	defer func() {
		s.Stop()
		// Shutdown the RPC server if it's not disabled.
		if !conf.Cfg.P2PNet.DisableRPC {
			rpcServer.Stop()
		}
	}()
	go func() {
		<- rpcServer.RequestedProcessShutdown()
		shutdownRequestChannel <- struct{}{}
	}()
	<-interrupt
	return nil
}

func main() {
	// Use all processor cores.
	runtime.GOMAXPROCS(runtime.NumCPU())

	// Block and transaction processing can cause bursty allocations.  This
	// limits the garbage collector from excessively overallocating during
	// bursts.  This value was arrived at with the help of profiling live
	// usage.
	debug.SetGCPercent(10)

	// Up some limits.
	if err := limits.SetLimits(); err != nil {
		fmt.Fprintf(os.Stderr, "failed to set limits: %v\n", err)
		os.Exit(1)
	}

	// Work around defer not working after os.Exit()
	if err := bchMain(context.Background()); err != nil {
		os.Exit(1)
	}
}
