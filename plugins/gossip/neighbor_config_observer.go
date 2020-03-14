package gossip

import (
	"strings"

	"github.com/fsnotify/fsnotify"
	"github.com/pkg/errors"

	"github.com/iotaledger/hive.go/iputils"

	"github.com/gohornet/hornet/packages/config"
)

func configureConfigObserver() {
	config.NeighborsConfig.WatchConfig()
}

func runConfigObserver() {
	config.NeighborsConfig.OnConfigChange(func(e fsnotify.Event) {
		if !config.IsNeighborsConfigHotReloadAllowed() {
			return
		}

		// whether to accept any incoming neighbor connection
		acceptAnyNeighborConnectionRead := config.NeighborsConfig.GetBool(config.CfgNeighborsAcceptAnyNeighborConnection)
		if acceptAnyNeighborConnection != acceptAnyNeighborConnectionRead {
			gossipLogger.Infof("Set acceptAnyNeighborConnection to <%v> due to config change", acceptAnyNeighborConnectionRead)
			acceptAnyNeighborConnection = acceptAnyNeighborConnectionRead
		}

		modified, added, removed := getNeighborConfigDiff()

		// Modify neighbors
		if len(modified) > 0 {
			gossipLogger.Infof("Modify neighbors due to config change")
			for _, nb := range modified {
				if err := RemoveNeighbor(nb.Identity); err != nil {
					gossipLogger.Warn(err)
				}
			}
			addNewNeighbors(modified)
		}

		// Add neighbors
		if len(added) > 0 {
			gossipLogger.Infof("Add neighbors due to config change")
			addNewNeighbors(added)
		}

		// Remove Neighbors
		if len(removed) > 0 {
			for _, nb := range removed {
				if err := RemoveNeighbor(nb.Identity); err != nil {
					gossipLogger.Warnf("Remove neighbor due to config change failed with: %v", err)
				} else {
					gossipLogger.Infof("Remove neighbor (%s) due to config change was successful", nb.Identity)
				}
			}
		}
	})
}

// calculates the differences between the loaded neighbors and the modified config
func getNeighborConfigDiff() (modified, added, removed []config.NeighborConfig) {
	boundNeighbors := GetNeighbors()
	configNeighbors := []config.NeighborConfig{}
	if err := config.NeighborsConfig.UnmarshalKey(config.CfgNeighbors, &configNeighbors); err != nil {
		gossipLogger.Error(err)
	}

	for _, boundNeighbor := range boundNeighbors {
		found := false
		for _, configNeighbor := range configNeighbors {
			if strings.EqualFold(boundNeighbor.Address, configNeighbor.Identity) || strings.EqualFold(boundNeighbor.DomainWithPort, configNeighbor.Identity) {
				found = true
				if (boundNeighbor.PreferIPv6 != configNeighbor.PreferIPv6) || (boundNeighbor.Alias != configNeighbor.Alias) {
					modified = append(modified, configNeighbor)
				}
			}
		}
		if !found {
			removed = append(removed, config.NeighborConfig{Identity: boundNeighbor.Address, PreferIPv6: boundNeighbor.PreferIPv6})
		}
	}

	for _, configNeighbor := range configNeighbors {

		if configNeighbor.Identity == ExampleNeighborIdentity {
			// Ignore the example neighbor
			continue
		}

		found := false
		for _, boundNeighbor := range boundNeighbors {
			if strings.EqualFold(boundNeighbor.Address, configNeighbor.Identity) || strings.EqualFold(boundNeighbor.DomainWithPort, configNeighbor.Identity) {
				found = true
			}
		}
		if !found {
			added = append(added, configNeighbor)
		}
	}
	return
}

func addNewNeighbors(neighbors []config.NeighborConfig) {
	neighborsLock.Lock()
	defer neighborsLock.Unlock()
	for _, nb := range neighbors {
		if nb.Identity == "" {
			continue
		}

		if nb.Identity == ExampleNeighborIdentity {
			// Ignore the example neighbor
			continue
		}

		// check whether already in reconnect pool
		if _, exists := reconnectPool[nb.Identity]; exists {
			gossipLogger.Error(errors.Wrapf(ErrNeighborAlreadyKnown, "%s is already known and in the reconnect pool", nb.Identity))
			continue
		}

		originAddr, err := iputils.ParseOriginAddress(nb.Identity)
		if err != nil {
			gossipLogger.Error(errors.Wrapf(err, "invalid neighbor address %s", nb.Identity))
			continue
		}
		originAddr.PreferIPv6 = nb.PreferIPv6
		originAddr.Alias = nb.Alias

		addNeighborToReconnectPool(&reconnectneighbor{OriginAddr: originAddr})
	}
	wakeupReconnectPool()
}
