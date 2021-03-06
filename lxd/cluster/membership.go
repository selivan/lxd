package cluster

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"github.com/CanonicalLtd/raft-http"
	"github.com/CanonicalLtd/raft-membership"
	"github.com/hashicorp/raft"
	"github.com/lxc/lxd/lxd/db"
	"github.com/lxc/lxd/lxd/db/cluster"
	"github.com/lxc/lxd/lxd/node"
	"github.com/lxc/lxd/lxd/state"
	"github.com/lxc/lxd/lxd/util"
	"github.com/lxc/lxd/shared"
	"github.com/lxc/lxd/shared/api"
	"github.com/lxc/lxd/shared/log15"
	"github.com/lxc/lxd/shared/logger"
	"github.com/pkg/errors"
)

// Bootstrap turns a non-clustered LXD instance into the first (and leader)
// node of a new LXD cluster.
//
// This instance must already have its core.https_address set and be listening
// on the associated network address.
func Bootstrap(state *state.State, gateway *Gateway, name string) error {
	// Check parameters
	if name == "" {
		return fmt.Errorf("node name must not be empty")
	}

	err := membershipCheckNoLeftoverClusterCert(state.OS.VarDir)
	if err != nil {
		return err
	}

	var address string
	err = state.Node.Transaction(func(tx *db.NodeTx) error {
		// Fetch current network address and raft nodes
		config, err := node.ConfigLoad(tx)
		if err != nil {
			return errors.Wrap(err, "failed to fetch node configuration")
		}
		address = config.HTTPSAddress()

		// Make sure node-local database state is in order.
		err = membershipCheckNodeStateForBootstrapOrJoin(tx, address)
		if err != nil {
			return err
		}

		// Add ourselves as first raft node
		err = tx.RaftNodeFirst(address)
		if err != nil {
			return errors.Wrap(err, "failed to insert first raft node")
		}

		return nil
	})
	if err != nil {
		return err
	}

	// Update our own entry in the nodes table.
	err = state.Cluster.Transaction(func(tx *db.ClusterTx) error {
		// Make sure cluster database state is in order.
		err := membershipCheckClusterStateForBootstrapOrJoin(tx)
		if err != nil {
			return err
		}

		// Add ourselves to the nodes table.
		err = tx.NodeUpdate(1, name, address)
		if err != nil {
			return errors.Wrap(err, "failed to update cluster node")
		}

		return nil
	})
	if err != nil {
		return err
	}

	// Shutdown the gateway. This will trash any gRPC SQL connection
	// against our in-memory dqlite driver and shutdown the associated raft
	// instance. We also lock regular access to the cluster database since
	// we don't want any other database code to run while we're
	// reconfiguring raft.
	err = state.Cluster.EnterExclusive()
	if err != nil {
		return errors.Wrap(err, "failed to acquire cluster database lock")
	}

	err = gateway.Shutdown()
	if err != nil {
		return errors.Wrap(err, "failed to shutdown gRPC SQL gateway")
	}

	// Re-initialize the gateway. This will create a new raft factory an
	// dqlite driver instance, which will be exposed over gRPC by the
	// gateway handlers.
	err = gateway.init()
	if err != nil {
		return errors.Wrap(err, "failed to re-initialize gRPC SQL gateway")
	}
	err = gateway.waitLeadership()
	if err != nil {
		return err
	}

	// The cluster certificates are symlinks against the regular node
	// certificate.
	for _, ext := range []string{".crt", ".key", ".ca"} {
		if ext == ".ca" && !shared.PathExists(filepath.Join(state.OS.VarDir, "server.ca")) {
			continue
		}
		err := os.Symlink("server"+ext, filepath.Join(state.OS.VarDir, "cluster"+ext))
		if err != nil {
			return errors.Wrap(err, "failed to create cluster cert symlink")
		}
	}

	// Make sure we can actually connect to the cluster database through
	// the network endpoint. This also releases the previously acquired
	// lock and makes the Go SQL pooling system invalidate the old
	// connection, so new queries will be executed over the new gRPC
	// network connection.
	err = state.Cluster.ExitExclusive(func(tx *db.ClusterTx) error {
		_, err := tx.Nodes()
		return err
	})
	if err != nil {
		return errors.Wrap(err, "cluster database initialization failed")
	}

	return nil
}

// Accept a new node and add it to the cluster.
//
// This instance must already be clustered.
//
// Return an updated list raft database nodes (possibly including the newly
// accepted node).
func Accept(state *state.State, gateway *Gateway, name, address string, schema, api int) ([]db.RaftNode, error) {
	// Check parameters
	if name == "" {
		return nil, fmt.Errorf("node name must not be empty")
	}
	if address == "" {
		return nil, fmt.Errorf("node address must not be empty")
	}

	// Insert the new node into the nodes table.
	err := state.Cluster.Transaction(func(tx *db.ClusterTx) error {
		// Check that the node can be accepted with these parameters.
		err := membershipCheckClusterStateForAccept(tx, name, address, schema, api)
		if err != nil {
			return err
		}
		// Add the new node
		id, err := tx.NodeAdd(name, address)
		if err != nil {
			return errors.Wrap(err, "failed to insert new node")
		}

		// Mark the node as pending, so it will be skipped when
		// performing heartbeats or sending cluster
		// notifications.
		err = tx.NodePending(id, true)
		if err != nil {
			return errors.Wrap(err, "failed to mark new node as pending")
		}

		return nil
	})
	if err != nil {
		return nil, err
	}

	// Possibly insert the new node into the raft_nodes table (if we have
	// less than 3 database nodes).
	nodes, err := gateway.currentRaftNodes()
	if err != nil {
		return nil, errors.Wrap(err, "failed to get raft nodes from the log")
	}
	if len(nodes) < membershipMaxRaftNodes {
		err = state.Node.Transaction(func(tx *db.NodeTx) error {
			id, err := tx.RaftNodeAdd(address)
			if err != nil {
				return err
			}
			nodes = append(nodes, db.RaftNode{ID: id, Address: address})
			return nil
		})
		if err != nil {
			return nil, errors.Wrap(err, "failed to insert new node into raft_nodes")
		}
	}

	return nodes, nil
}

// Join makes a non-clustered LXD node join an existing cluster.
//
// It's assumed that Accept() was previously called against the target node,
// which handed the raft server ID.
//
// The cert parameter must contain the keypair/CA material of the cluster being
// joined.
func Join(state *state.State, gateway *Gateway, cert *shared.CertInfo, name string, nodes []db.RaftNode) error {
	// Check parameters
	if name == "" {
		return fmt.Errorf("node name must not be empty")
	}

	var address string
	err := state.Node.Transaction(func(tx *db.NodeTx) error {
		// Fetch current network address and raft nodes
		config, err := node.ConfigLoad(tx)
		if err != nil {
			return errors.Wrap(err, "failed to fetch node configuration")
		}
		address = config.HTTPSAddress()

		// Make sure node-local database state is in order.
		err = membershipCheckNodeStateForBootstrapOrJoin(tx, address)
		if err != nil {
			return err
		}

		// Set the raft nodes list to the one that was returned by Accept().
		err = tx.RaftNodesReplace(nodes)
		if err != nil {
			return errors.Wrap(err, "failed to set raft nodes")
		}

		return nil
	})
	if err != nil {
		return err
	}

	// Get the local config keys for the cluster pools and networks. It
	// assumes that the local storage pools and networks match the cluster
	// networks, if not an error will be returned. Also get any outstanding
	// operation, typically there will be just one, created by the POST
	// /cluster/nodes request which triggered this code.
	var pools map[string]map[string]string
	var networks map[string]map[string]string
	var operations []string
	err = state.Cluster.Transaction(func(tx *db.ClusterTx) error {
		pools, err = tx.StoragePoolConfigs()
		if err != nil {
			return err
		}
		networks, err = tx.NetworkConfigs()
		if err != nil {
			return err
		}
		operations, err = tx.OperationsUUIDs()
		if err != nil {
			return err
		}
		return nil
	})
	if err != nil {
		return err
	}

	// Lock regular access to the cluster database since we don't want any
	// other database code to run while we're reconfiguring raft.
	err = state.Cluster.EnterExclusive()
	if err != nil {
		return errors.Wrap(err, "failed to acquire cluster database lock")
	}

	// Shutdown the gateway and wipe any raft data. This will trash any
	// gRPC SQL connection against our in-memory dqlite driver and shutdown
	// the associated raft instance.
	err = gateway.Shutdown()
	if err != nil {
		return errors.Wrap(err, "failed to shutdown gRPC SQL gateway")
	}
	err = os.RemoveAll(filepath.Join(state.OS.VarDir, "raft"))
	if err != nil {
		return errors.Wrap(err, "failed to remove existing raft data")
	}

	// Re-initialize the gateway. This will create a new raft factory an
	// dqlite driver instance, which will be exposed over gRPC by the
	// gateway handlers.
	gateway.cert = cert
	err = gateway.init()
	if err != nil {
		return errors.Wrap(err, "failed to re-initialize gRPC SQL gateway")
	}

	// If we are listed among the database nodes, join the raft cluster.
	id := ""
	target := ""
	for _, node := range nodes {
		if node.Address == address {
			id = strconv.Itoa(int(node.ID))
		} else {
			target = node.Address
		}
	}
	if id != "" {
		logger.Info(
			"Joining dqlite raft cluster",
			log15.Ctx{"id": id, "address": address, "target": target})
		changer := gateway.raft.MembershipChanger()
		err := changer.Join(raft.ServerID(id), raft.ServerAddress(target), 5*time.Second)
		if err != nil {
			return err
		}
	} else {
		logger.Info("Joining cluster as non-database node")
	}

	// Make sure we can actually connect to the cluster database through
	// the network endpoint. This also releases the previously acquired
	// lock and makes the Go SQL pooling system invalidate the old
	// connection, so new queries will be executed over the new gRPC
	// network connection. Also, update the storage_pools and networks
	// tables with our local configuration.
	logger.Info("Migrate local data to cluster database")
	err = state.Cluster.ExitExclusive(func(tx *db.ClusterTx) error {
		node, err := tx.NodePendingByAddress(address)
		if err != nil {
			return errors.Wrap(err, "failed to get ID of joining node")
		}
		state.Cluster.NodeID(node.ID)
		tx.NodeID(node.ID)

		// Storage pools.
		ids, err := tx.StoragePoolIDsNotPending()
		if err != nil {
			return errors.Wrap(err, "failed to get cluster storage pool IDs")
		}
		for name, id := range ids {
			config, ok := pools[name]
			if !ok {
				return fmt.Errorf("joining node has no config for pool %s", name)
			}
			err := tx.StoragePoolNodeJoin(id, node.ID)
			if err != nil {
				return errors.Wrap(err, "failed to add joining node's to the pool")
			}
			// We only need to add the node-specific keys, since
			// the other keys are global and are already there.
			for key := range config {
				if !shared.StringInSlice(key, db.StoragePoolNodeConfigKeys) {
					delete(config, key)
				}
			}
			err = tx.StoragePoolConfigAdd(id, node.ID, config)
			if err != nil {
				return errors.Wrap(err, "failed to add joining node's pool config")
			}
		}

		// Networks.
		ids, err = tx.NetworkIDsNotPending()
		if err != nil {
			return errors.Wrap(err, "failed to get cluster network IDs")
		}
		for name, id := range ids {
			config, ok := networks[name]
			if !ok {
				return fmt.Errorf("joining node has no config for network %s", name)
			}
			err := tx.NetworkNodeJoin(id, node.ID)
			if err != nil {
				return errors.Wrap(err, "failed to add joining node's to the network")
			}
			// We only need to add the node-specific keys, since
			// the other keys are global and are already there.
			for key := range config {
				if !shared.StringInSlice(key, db.NetworkNodeConfigKeys) {
					delete(config, key)
				}
			}
			err = tx.NetworkConfigAdd(id, node.ID, config)
			if err != nil {
				return errors.Wrap(err, "failed to add joining node's network config")
			}
		}

		// Migrate outstanding operations.
		for _, uuid := range operations {
			_, err := tx.OperationAdd(uuid)
			if err != nil {
				return errors.Wrapf(err, "failed to migrate operation %s", uuid)
			}
		}

		// Remove the pending flag for ourselves
		// notifications.
		err = tx.NodePending(node.ID, false)
		if err != nil {
			return errors.Wrapf(err, "failed to unmark the node as pending")
		}

		return nil
	})
	if err != nil {
		return errors.Wrap(err, "cluster database initialization failed")
	}

	return nil
}

// Leave a cluster.
//
// If the force flag is true, the node will leave even if it still has
// containers and images.
//
// The node will only leave the raft cluster, and won't be removed from the
// database. That's done by Purge().
//
// Upon success, return the address of the leaving node.
func Leave(state *state.State, gateway *Gateway, name string, force bool) (string, error) {
	logger.Debugf("Make node %s leave the cluster", name)

	// Check if the node can be deleted and track its address.
	var address string
	err := state.Cluster.Transaction(func(tx *db.ClusterTx) error {
		// Get the node (if it doesn't exists an error is returned).
		node, err := tx.NodeByName(name)
		if err != nil {
			return err
		}

		// Check that the node is eligeable for leaving.
		if !force {
			err := membershipCheckClusterStateForLeave(tx, node.ID)
			if err != nil {
				return err
			}
		}

		address = node.Address
		return nil
	})
	if err != nil {
		return "", err
	}

	// If the node is a database node, leave the raft cluster too.
	id := ""
	target := ""
	err = state.Node.Transaction(func(tx *db.NodeTx) error {
		nodes, err := tx.RaftNodes()
		if err != nil {
			return err
		}
		for i, node := range nodes {
			if node.Address == address {
				id = strconv.Itoa(int(node.ID))
				// Save the address of another database node,
				// we'll use it to leave the raft cluster.
				target = nodes[(i+1)%len(nodes)].Address
				break
			}
		}
		return nil
	})
	if err != nil {
		return "", err
	}

	if target != "" {
		logger.Info(
			"Remove node from dqlite raft cluster",
			log15.Ctx{"id": id, "address": address, "target": target})
		dial, err := raftDial(gateway.cert)
		if err != nil {
			return "", err
		}
		err = rafthttp.ChangeMembership(
			raftmembership.LeaveRequest, raftEndpoint, dial,
			raft.ServerID(id), address, target, 5*time.Second)
		if err != nil {
			return "", err
		}
	}

	return address, nil
}

// Purge removes a node entirely from the cluster database.
func Purge(cluster *db.Cluster, name string) error {
	logger.Debugf("Remove node %s from the database", name)

	return cluster.Transaction(func(tx *db.ClusterTx) error {
		// Get the node (if it doesn't exists an error is returned).
		node, err := tx.NodeByName(name)
		if err != nil {
			return errors.Wrapf(err, "failed to get node %s", name)
		}

		err = tx.NodeClear(node.ID)
		if err != nil {
			return errors.Wrapf(err, "failed to clear node %s", name)
		}

		err = tx.NodeRemove(node.ID)
		if err != nil {
			return errors.Wrapf(err, "failed to remove node %s", name)
		}
		return nil
	})
}

// List the nodes of the cluster.
func List(state *state.State) ([]api.ClusterMember, error) {
	addresses := []string{} // Addresses of database nodes
	err := state.Node.Transaction(func(tx *db.NodeTx) error {
		nodes, err := tx.RaftNodes()
		if err != nil {
			return errors.Wrap(err, "failed to fetch current raft nodes")
		}
		for _, node := range nodes {
			addresses = append(addresses, node.Address)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}

	var nodes []db.NodeInfo
	var offlineThreshold time.Duration

	err = state.Cluster.Transaction(func(tx *db.ClusterTx) error {
		nodes, err = tx.Nodes()
		if err != nil {
			return err
		}
		offlineThreshold, err = tx.NodeOfflineThreshold()
		if err != nil {
			return err
		}

		return nil
	})
	if err != nil {
		return nil, err
	}

	result := make([]api.ClusterMember, len(nodes))
	now := time.Now()
	version := nodes[0].Version()
	for i, node := range nodes {
		result[i].ServerName = node.Name
		result[i].URL = fmt.Sprintf("https://%s", node.Address)
		result[i].Database = shared.StringInSlice(node.Address, addresses)
		if node.IsOffline(offlineThreshold) {
			result[i].Status = "Offline"
			result[i].Message = fmt.Sprintf(
				"no heartbeat since %s", now.Sub(node.Heartbeat))
		} else {
			result[i].Status = "Online"
			result[i].Message = "fully operational"
		}

		n, err := util.CompareVersions(version, node.Version())
		if err != nil {
			result[i].Status = "Broken"
			result[i].Message = "inconsistent version"
			continue
		}

		if n == 1 {
			// This node's version is lower, which means the
			// version that the previous node in the loop has been
			// upgraded.
			version = node.Version()
		}
	}

	// Update the state of online nodes that have been upgraded and whose
	// schema is more recent than the rest of the nodes.
	for i, node := range nodes {
		if result[i].Status != "Online" {
			continue
		}
		n, err := util.CompareVersions(version, node.Version())
		if err != nil {
			continue
		}
		if n == 2 {
			result[i].Status = "Blocked"
			result[i].Message = "waiting for other nodes to be upgraded"
		}
	}

	return result, nil
}

// Count is a convenience for checking the current number of nodes in the
// cluster.
func Count(state *state.State) (int, error) {
	var count int
	err := state.Cluster.Transaction(func(tx *db.ClusterTx) error {
		var err error
		count, err = tx.NodesCount()
		return err
	})
	return count, err
}

// Enabled is a convenience that returns true if clustering is enabled on this
// node.
func Enabled(node *db.Node) (bool, error) {
	enabled := false
	err := node.Transaction(func(tx *db.NodeTx) error {
		addresses, err := tx.RaftNodeAddresses()
		if err != nil {
			return err
		}
		enabled = len(addresses) > 0
		return nil
	})
	return enabled, err
}

// Check that node-related preconditions are met for bootstrapping or joining a
// cluster.
func membershipCheckNodeStateForBootstrapOrJoin(tx *db.NodeTx, address string) error {
	nodes, err := tx.RaftNodes()
	if err != nil {
		return errors.Wrap(err, "failed to fetch current raft nodes")
	}

	hasNetworkAddress := address != ""
	hasRaftNodes := len(nodes) > 0

	// Sanity check that we're not in an inconsistent situation, where no
	// network address is set, but still there are entries in the
	// raft_nodes table.
	if !hasNetworkAddress && hasRaftNodes {
		return fmt.Errorf("inconsistent state: found leftover entries in raft_nodes")
	}

	if !hasNetworkAddress {
		return fmt.Errorf("no core.https_address config is set on this node")
	}
	if hasRaftNodes {
		return fmt.Errorf("the node is already part of a cluster")
	}

	return nil
}

// Check that cluster-related preconditions are met for bootstrapping or
// joining a cluster.
func membershipCheckClusterStateForBootstrapOrJoin(tx *db.ClusterTx) error {
	nodes, err := tx.Nodes()
	if err != nil {
		return errors.Wrap(err, "failed to fetch current cluster nodes")
	}
	if len(nodes) != 1 {
		return fmt.Errorf("inconsistent state: found leftover entries in nodes")
	}
	return nil
}

// Check that cluster-related preconditions are met for accepting a new node.
func membershipCheckClusterStateForAccept(tx *db.ClusterTx, name string, address string, schema int, api int) error {
	nodes, err := tx.Nodes()
	if err != nil {
		return errors.Wrap(err, "failed to fetch current cluster nodes")
	}
	if len(nodes) == 1 && nodes[0].Address == "0.0.0.0" {
		return fmt.Errorf("clustering not enabled")
	}

	for _, node := range nodes {
		if node.Name == name {
			return fmt.Errorf("cluster already has node with name %s", name)
		}
		if node.Address == address {
			return fmt.Errorf("cluster already has node with address %s", address)
		}
		if node.Schema != schema {
			return fmt.Errorf("schema version mismatch: cluster has %d", node.Schema)
		}
		if node.APIExtensions != api {
			return fmt.Errorf("API version mismatch: cluster has %d", node.APIExtensions)
		}
	}

	return nil
}

// Check that cluster-related preconditions are met for leaving a cluster.
func membershipCheckClusterStateForLeave(tx *db.ClusterTx, nodeID int64) error {
	// Check that it has no containers or images.
	message, err := tx.NodeIsEmpty(nodeID)
	if err != nil {
		return err
	}
	if message != "" {
		return fmt.Errorf(message)
	}

	// Check that it's not the last node.
	nodes, err := tx.Nodes()
	if err != nil {
		return err
	}
	if len(nodes) == 1 {
		return fmt.Errorf("node is the only node in the cluster")
	}
	return nil
}

// Check that there is no left-over cluster certificate in the LXD var dir of
// this node.
func membershipCheckNoLeftoverClusterCert(dir string) error {
	// Sanity check that there's no leftover cluster certificate
	for _, basename := range []string{"cluster.crt", "cluster.key", "cluster.ca"} {
		if shared.PathExists(filepath.Join(dir, basename)) {
			return fmt.Errorf("inconsistent state: found leftover cluster certificate")
		}
	}
	return nil
}

// SchemaVersion holds the version of the cluster database schema.
var SchemaVersion = cluster.SchemaVersion

// We currently aim at having 3 nodes part of the raft dqlite cluster.
//
// TODO: this number should probably be configurable.
const membershipMaxRaftNodes = 3
