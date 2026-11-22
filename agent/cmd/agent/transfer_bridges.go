package main

import (
	"context"
	"log"
	"time"

	"phoneagent/internal/ari"
	"phoneagent/internal/call"
)

// A final bridge belongs to the event coordinator, not the finished assistant.
// Its expected phone channels remain owned until both have ended. A failed
// DELETE keeps the empty bridge here so reconciliation can try again.
type transferBridge struct {
	channels map[string]struct{}
	deleting bool
}

func (c *coordinator) ownTransferBridges(h *call.Handler, client *ari.Client) {
	h.SetTransferBridgeCallbacks(c.trackTransferBridge, func(id string) {
		c.abandonTransferBridge(id)
		c.cleanupTransferBridges(client)
	})
}

func (c *coordinator) trackTransferBridge(id, caller, recipient string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.transferBridges[id] = &transferBridge{channels: map[string]struct{}{caller: {}, recipient: {}}}
}

func (c *coordinator) abandonTransferBridge(id string) {
	c.mu.Lock()
	if bridge := c.transferBridges[id]; bridge != nil {
		bridge.channels = nil
	}
	c.mu.Unlock()
}

func (c *coordinator) transferChannelEnded(cid string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	ready := false
	for _, bridge := range c.transferBridges {
		delete(bridge.channels, cid)
		ready = ready || (len(bridge.channels) == 0 && !bridge.deleting)
	}
	return ready
}

func (c *coordinator) transferBridgeSnapshot() map[string]*transferBridge {
	c.mu.Lock()
	defer c.mu.Unlock()
	snapshot := make(map[string]*transferBridge, len(c.transferBridges))
	for id, bridge := range c.transferBridges {
		snapshot[id] = bridge
	}
	return snapshot
}

func (c *coordinator) reconcileTransferBridges(client *ari.Client, live map[string]struct{}, snapshot map[string]*transferBridge) {
	c.mu.Lock()
	for id, bridge := range snapshot {
		// A bridge registered during the HTTP inventory may refer to channels
		// created after that inventory began. Wait for a fresh snapshot for it.
		if c.transferBridges[id] != bridge {
			continue
		}
		for cid := range bridge.channels {
			if _, ok := live[cid]; !ok {
				delete(bridge.channels, cid)
			}
		}
	}
	c.mu.Unlock()
	c.cleanupTransferBridges(client)
}

func (c *coordinator) syncTransferBridges(client *ari.Client) {
	snapshot := c.transferBridgeSnapshot()
	if len(snapshot) == 0 {
		return
	}
	live, err := client.LiveChannelIDs()
	if err != nil {
		log.Printf("transfer: cannot reconcile bridge channels: %v", err)
		return
	}
	c.reconcileTransferBridges(client, live, snapshot)
}

func (c *coordinator) cleanupTransferBridges(client *ari.Client) {
	type candidate struct {
		id     string
		bridge *transferBridge
	}
	var work []candidate
	c.mu.Lock()
	for id, bridge := range c.transferBridges {
		if len(bridge.channels) == 0 && !bridge.deleting {
			bridge.deleting = true
			work = append(work, candidate{id, bridge})
		}
	}
	c.mu.Unlock()
	for _, item := range work {
		err := client.Delete("/bridges/" + item.id)
		gone := err == nil
		if !gone {
			_, status, lookupErr := client.Get("/bridges/" + item.id)
			gone = lookupErr == nil && status == 404
		}
		c.mu.Lock()
		if c.transferBridges[item.id] == item.bridge {
			if gone {
				delete(c.transferBridges, item.id)
			} else {
				item.bridge.deleting = false
			}
		}
		c.mu.Unlock()
		if !gone {
			log.Printf("transfer: bridge %s cleanup could not be confirmed: %v", item.id, err)
		}
	}
}

// ChannelDestroyed usually closes the ledger immediately. This small sweep also
// covers missed events across an ARI reconnect, including deployments without DB.
func (c *coordinator) monitorTransferBridges(ctx context.Context, client *ari.Client) {
	ticker := time.NewTicker(outboundPollInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		c.syncTransferBridges(client)
	}
}
