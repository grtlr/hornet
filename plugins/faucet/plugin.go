package faucet

import (
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/labstack/echo/v4"
	"github.com/labstack/echo/v4/middleware"
	"go.uber.org/dig"
	"golang.org/x/time/rate"

	"github.com/gohornet/hornet/core/gracefulshutdown"
	"github.com/gohornet/hornet/pkg/model/faucet"
	"github.com/gohornet/hornet/pkg/model/storage"
	"github.com/gohornet/hornet/pkg/model/utxo"
	"github.com/gohornet/hornet/pkg/node"
	"github.com/gohornet/hornet/pkg/pow"
	"github.com/gohornet/hornet/pkg/protocol/gossip"
	"github.com/gohornet/hornet/pkg/restapi"
	"github.com/gohornet/hornet/pkg/shutdown"
	"github.com/gohornet/hornet/pkg/tipselect"
	"github.com/gohornet/hornet/pkg/utils"
	"github.com/iotaledger/hive.go/configuration"
	iotago "github.com/iotaledger/iota.go/v2"
	"github.com/iotaledger/iota.go/v2/ed25519"
)

const (

	// RouteFaucetInfo is the route to give info about the faucet address.
	// GET returns address and balance of the faucet.
	RouteFaucetInfo = "/info"

	// RouteFaucetEnqueue is the route to tell the faucet to pay out some funds to the given address.
	// POST enqueues a new request.
	RouteFaucetEnqueue = "/enqueue"
)

func init() {
	Plugin = &node.Plugin{
		Status: node.StatusDisabled,
		Pluggable: node.Pluggable{
			Name:      "Faucet",
			DepsFunc:  func(cDeps dependencies) { deps = cDeps },
			Params:    params,
			Provide:   provide,
			Configure: configure,
			Run:       run,
		},
	}
}

var (
	Plugin *node.Plugin
	deps   dependencies
)

type dependencies struct {
	dig.In
	Faucet *faucet.Faucet
	Echo   *echo.Echo
}

func provide(c *dig.Container) {

	privateKeys, err := utils.LoadEd25519PrivateKeysFromEnvironment("FAUCET_PRV_KEY")
	if err != nil {
		Plugin.Panicf("loading faucet private key failed, err: %s", err)
	}

	if len(privateKeys) == 0 {
		Plugin.Panic("loading faucet private key failed, err: no private keys given")
	}

	if len(privateKeys) > 1 {
		Plugin.Panic("loading faucet private key failed, err: too many private keys given")
	}

	privateKey := privateKeys[0]
	if len(privateKey) != ed25519.PrivateKeySize {
		Plugin.Panic("loading faucet private key failed, err: wrong private key length")
	}

	faucetAddress := iotago.AddressFromEd25519PubKey(privateKey.Public().(ed25519.PublicKey))
	faucetSigner := iotago.NewInMemoryAddressSigner(iotago.NewAddressKeysForEd25519Address(&faucetAddress, privateKey))

	type faucetdeps struct {
		dig.In
		Storage          *storage.Storage
		PowHandler       *pow.Handler
		UTXO             *utxo.Manager
		NodeConfig       *configuration.Configuration `name:"nodeConfig"`
		NetworkID        uint64                       `name:"networkId"`
		BelowMaxDepth    int                          `name:"belowMaxDepth"`
		Bech32HRP        iotago.NetworkPrefix         `name:"bech32HRP"`
		TipSelector      *tipselect.TipSelector
		MessageProcessor *gossip.MessageProcessor
	}

	if err := c.Provide(func(deps faucetdeps) *faucet.Faucet {
		return faucet.New(
			deps.Storage,
			deps.NetworkID,
			deps.BelowMaxDepth,
			deps.UTXO,
			&faucetAddress,
			faucetSigner,
			deps.TipSelector.SelectNonLazyTips,
			deps.PowHandler,
			deps.MessageProcessor.Emit,
			faucet.WithLogger(Plugin.Logger()),
			faucet.WithHRPNetworkPrefix(deps.Bech32HRP),
			faucet.WithAmount(uint64(deps.NodeConfig.Int64(CfgFaucetAmount))),
			faucet.WithSmallAmount(uint64(deps.NodeConfig.Int64(CfgFaucetSmallAmount))),
			faucet.WithMaxAddressBalance(uint64(deps.NodeConfig.Int64(CfgFaucetMaxAddressBalance))),
			faucet.WithMaxOutputCount(deps.NodeConfig.Int(CfgFaucetMaxOutputCount)),
			faucet.WithIndexationMessage(deps.NodeConfig.String(CfgFaucetIndexationMessage)),
			faucet.WithBatchTimeout(deps.NodeConfig.Duration(CfgFaucetBatchTimeout)),
			faucet.WithPowWorkerCount(deps.NodeConfig.Int(CfgFaucetPoWWorkerCount)),
		)
	}); err != nil {
		Plugin.Panic(err)
	}
}

func configure() {
	routeGroup := deps.Echo.Group("/api/plugins/faucet")

	allowedRoutes := map[string][]string{
		http.MethodGet: {
			"/api/plugins/faucet/info",
		},
	}

	rateLimiterSkipper := func(context echo.Context) bool {
		// Check for which route we will skip the rate limiter
		routesForMethod, exists := allowedRoutes[context.Request().Method]
		if !exists {
			return false
		}

		path := context.Request().URL.EscapedPath()
		for _, prefix := range routesForMethod {
			if strings.HasPrefix(path, prefix) {
				return true
			}
		}

		return false
	}

	rateLimiterConfig := middleware.RateLimiterConfig{
		Skipper: rateLimiterSkipper,
		Store: middleware.NewRateLimiterMemoryStoreWithConfig(
			middleware.RateLimiterMemoryStoreConfig{
				Rate:      rate.Limit(1 / 300.0), // 1 request every 5 minutes
				Burst:     10,                    // additional burst of 10 requests
				ExpiresIn: 5 * time.Minute,
			},
		),
		IdentifierExtractor: func(ctx echo.Context) (string, error) {
			id := ctx.RealIP()
			return id, nil
		},
		ErrorHandler: func(context echo.Context, err error) error {
			return context.JSON(http.StatusForbidden, nil)
		},
		DenyHandler: func(context echo.Context, identifier string, err error) error {
			return context.JSON(http.StatusTooManyRequests, nil)
		},
	}
	routeGroup.Use(middleware.RateLimiterWithConfig(rateLimiterConfig))

	routeGroup.GET(RouteFaucetInfo, func(c echo.Context) error {
		resp, err := getFaucetInfo(c)
		if err != nil {
			return err
		}

		return restapi.JSONResponse(c, http.StatusOK, resp)
	})

	routeGroup.POST(RouteFaucetEnqueue, func(c echo.Context) error {
		resp, err := addFaucetOutputToQueue(c)
		if err != nil {
			return err
		}

		return restapi.JSONResponse(c, http.StatusAccepted, resp)
	})
}

func run() {
	// create a background worker that handles the enqueued faucet requests
	if err := Plugin.Daemon().BackgroundWorker("Faucet", func(shutdownSignal <-chan struct{}) {
		if err := deps.Faucet.RunFaucetLoop(shutdownSignal); err != nil {
			gracefulshutdown.SelfShutdown(fmt.Sprintf("faucet plugin hit a critical error: %s", err.Error()))
		}
	}, shutdown.PriorityFaucet); err != nil {
		Plugin.Panicf("failed to start worker: %s", err)
	}
}
