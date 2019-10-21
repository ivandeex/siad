package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"

	"gitlab.com/NebulousLabs/Sia/encoding"
	"gitlab.com/NebulousLabs/Sia/modules"
	"gitlab.com/NebulousLabs/Sia/modules/gateway"
	siaPersist "gitlab.com/NebulousLabs/Sia/persist"
	"gitlab.com/NebulousLabs/errors"
)

const nodeScannerDirName = "SiaNodeScanner"
const persistFileName = "persisted-node-set.json"

const maxSharedNodes = uint64(1000)
const maxRPCs = 10
const maxWorkers = 10
const workChSize = 1000

// pruneAge is the maxiumum allowed time in seconds since the last successful connection with a
// node before we remove it from the persisted set. It is 1 month in seconds.
// 1 hour * 24 hours/day * 30 days/month
const pruneAge = time.Hour * 24 * 30

const metadataHeader = "SiaNodeScanner Persisted Node Set"
const metadataVersion = "0.0.1"

var persistMetadata = siaPersist.Metadata{
	Header:  metadataHeader,
	Version: metadataVersion,
}

type nodeScanner struct {
	// The node scanner uses a dummy gateway to connect to nodes and
	// requests peers from nodes across the network using the
	// ShareNodes RPC.
	gateway *gateway.Gateway

	// Multiple workers are given addresses to scan using workCh.
	// The workers always send a result back to the main goroutine
	// using the resultCh
	workCh   chan workAssignment
	resultCh chan nodeScanResult

	// Count the total number of work assignments sent down workCh and the total
	// number of results received through resultCh.
	totalWorkAssignments int
	totalResults         int

	// The number of ShareNodes RPCs to make with each scanned node. Initially can
	// be set high (10) but should be lowered because the scan will waste a lot of
	// time receiving addresses it already knows.
	numRPCAttempts int

	// The seen set keeps track of all the addresses seen by the
	// scanner so far.
	seen map[modules.NetAddress]struct{}
	// The queue holds nodes to be added to workCh.
	queue []modules.NetAddress

	// Connection stats for the current scan.
	stats scannerStats

	// scanLog holds all the results for this scan.
	scanLog io.WriteCloser

	// data keeps track of connection time and uptime stats for each node that has
	// been succesfully connected to at least once it the past 30 days.
	data persistData
	// persistFile stores persistData using siaPersist.
	persistFile string

	testing bool
}

type persistData struct {
	// StartTime is the Unix timestamp of the first scan's start.
	StartTime time.Time

	// Keep connection time and uptime stats for each node.
	NodeStats map[modules.NetAddress]nodeStats
}

type nodeStats struct {
	// Timestamp of first succesful connection to this node.
	// Used for total uptime and uptime percentage calculations.
	FirstConnectionTime time.Time

	// Keep track of last time we successfully connected to each node.
	LastSuccessfulConnectionTime time.Time

	// RecentUptime counts the number of nanoseconds since the node was
	// last down (or since first time scanned if it hasn't failed yet).
	RecentUptime time.Duration

	// TotalUptime counts the total number of nanoseconds this node has been up since
	// the time of its first scan.
	TotalUptime time.Duration

	// UptimePercentage is TotalUptime divided by time since
	// FirstConnectionTime.
	UptimePercentage float64
}

// workAssignment tells a worker which node it should scan,
// and the number of times it should send the ShareNodes RPC.
// The ShareNodes RPC is used multiple times because nodes will
// only return 10 random peers, but we want as many as possible.
type workAssignment struct {
	node           modules.NetAddress
	maxRPCAttempts int
}

// nodeScanResult gives the set of nodes received from ShareNodes
// RPCs sent to a specific node. err is nil, an error from connecting,
// or an error from ShareNodes.
type nodeScanResult struct {
	Addr      modules.NetAddress
	Timestamp time.Time
	Err       error
	nodes     map[modules.NetAddress]struct{}
}

// Counters generated by the node scanner.
type scannerStats struct {
	// Counters for successful/unsuccessful connections.
	SuccessfulConnections int
	FailedConnections     int

	// Counters for common failure types.
	UnacceptableVersionFailures  int
	NetworkIsUnreachableFailures int
	NoRouteToHostFailures        int
	ConnectionRefusedFailures    int
	ConnectionTimedOutFailures   int
	AlreadyConnectedFailures     int
}

func main() {
	dirPtr := flag.String("dir", "", "Directory where the node scanner will store its results")
	flag.Parse()

	// Create a new nodeScanner and create new files and a gateway.
	ns := newNodeScanner(*dirPtr)

	// Inialize work queues and work/result channels.
	ns.initialize()

	// Start all workers and the main scan loop.
	ns.startScan()
}

// newNodeScanner creates a nodeScanner, creates the directories and files it
// uses while scanning, and initializes a gateway used for the scan.
func newNodeScanner(scannerDirPrefix string) (ns *nodeScanner) {
	ns = new(nodeScanner)
	ns.stats = scannerStats{}

	// Setup the node scanner's directories.
	scannerDirPath := filepath.Join(scannerDirPrefix, nodeScannerDirName)
	scannerGatewayDirPath := filepath.Join(scannerDirPath, "gateway")
	if _, err := os.Stat(scannerDirPath); os.IsNotExist(err) {
		err := os.Mkdir(scannerDirPath, 0777)
		if err != nil {
			log.Fatal("Error creating scan directory: ", err)
		}
	}
	if _, err := os.Stat(scannerGatewayDirPath); os.IsNotExist(err) {
		err := os.Mkdir(scannerGatewayDirPath, 0777)
		if err != nil {
			log.Fatal("Error creating scanner gateway directory: ", err)
		}
	}
	log.Printf("Logging data in:  %s\n", scannerDirPath)

	// Create the file for this scan.
	startTime := time.Now().Format("01-02:15:04")
	scanLogName := scannerDirPath + "/scan-" + startTime + ".json"
	scanLog, err := os.Create(scanLogName)
	if err != nil {
		log.Fatal("Error creating scan file: ", err)
	}
	ns.scanLog = scanLog

	// Create dummy gateway at localhost. It is used only to connect/disconnect
	// from nodes and to use the ShareNodes RPC with other nodes for the purposes
	// of this scan.
	g, err := gateway.New("localhost:0", true, scannerGatewayDirPath)
	if err != nil {
		log.Fatal("Error making new gateway: ", err)
	}
	log.Println("Set up gateway at address: ", g.Address())
	ns.gateway = g

	persistFilePath := filepath.Join(scannerDirPath, persistFileName)
	err = ns.setupPersistFile(persistFilePath)
	if err != nil {
		log.Fatal("Error setting up persist file: ", err)
	}
	return
}

// initialize uses the persisted set of nodes (if it exists) or the set
// of bootstrap peers to initialize the nodeScanner data structures used to give
// out worker assignments and to receive results.
func (ns *nodeScanner) initialize() {
	ns.numRPCAttempts = 5

	// If the persisted set is empty, start with bootstrap nodes in queue.
	// Otherwise start off with the persisted node set in the queue.
	if len(ns.data.NodeStats) == 0 {
		log.Println("Starting crawl with bootstrap peers")
		ns.queue = make([]modules.NetAddress, len(modules.BootstrapPeers))
		copy(ns.queue, modules.BootstrapPeers)
	} else {
		ns.queue = make([]modules.NetAddress, 0, len(ns.data.NodeStats))
		prunedPersistedData := persistData{
			StartTime: ns.data.StartTime,
			NodeStats: make(map[modules.NetAddress]nodeStats),
		}

		now := time.Now()
		for node, nodeStats := range ns.data.NodeStats {
			// Prune peers we haven't connected to in more than pruneAge
			// by not adding them to the new set.
			if now.Sub(nodeStats.LastSuccessfulConnectionTime) < pruneAge {
				prunedPersistedData.NodeStats[node] = nodeStats
				ns.queue = append(ns.queue, node)
			}
		}
		ns.data = prunedPersistedData
		log.Printf("Starting crawl with %d persisted peers\n", len(ns.data.NodeStats))
	}

	// Mark all starting nodes as seen.
	ns.seen = make(map[modules.NetAddress]struct{})
	for _, n := range ns.queue {
		ns.seen[n] = struct{}{}
	}
	ns.seen[ns.gateway.Address()] = struct{}{} // Don't scan yourself.

	// Setup worker channels and send initial queue items down.
	ns.workCh = make(chan workAssignment, workChSize)
	ns.resultCh = make(chan nodeScanResult, workChSize)

	var i int
	var node modules.NetAddress
	queueSize := len(ns.queue)
	for ; i < queueSize && i < cap(ns.workCh); i++ {
		ns.totalWorkAssignments++
		node, ns.queue = ns.queue[0], ns.queue[1:]
		ns.workCh <- workAssignment{
			node:           node,
			maxRPCAttempts: maxRPCs,
		}
	}
	log.Printf("Starting with %d nodes in workCh.\n", len(ns.workCh))
}

// startScan starts all workers and starts a main loop that reads from the
// resultCh, processes results, and creates new assignments for workers. This
// function is also responsible for updating all node stats, writing to the
// scanLog and updating the persistFile.
func (ns *nodeScanner) startScan() {
	// Start all the workers.
	for i := 0; i < maxWorkers; i++ {
		go startWorker(ns.gateway, ns.workCh, ns.resultCh)
	}

	// Print out stats periodically.
	// Persist the node set periodically.
	printTicker := time.NewTicker(10 * time.Second)
	persistTicker := time.NewTicker(10 * time.Second)

	for {
		select {
		case <-printTicker.C:
			fmt.Printf(ns.getStatsStr())

		case <-persistTicker.C:
			log.Println("Persisting nodes: ", len(ns.data.NodeStats))
			ns.persistData()

		case res := <-ns.resultCh:
			ns.totalResults++

			// Update persisted set with result.
			ns.updateNodeStats(res)

			// Add any new nodes from this set of results.
			for node := range res.nodes {
				if _, alreadySeen := ns.seen[node]; !alreadySeen {
					ns.seen[node] = struct{}{}
					ns.queue = append(ns.queue, node)
				}
			}

			// Log the result and any errors.
			ns.logWorkerResult(res)
		}

		// Fill up workCh with nodes from queue.
		var node modules.NetAddress
		for i := len(ns.workCh); i < cap(ns.workCh); i++ {
			if len(ns.queue) == 0 {
				break
			}
			node, ns.queue = ns.queue[len(ns.queue)-1], ns.queue[:len(ns.queue)-1]
			ns.totalWorkAssignments++
			ns.workCh <- workAssignment{
				node:           node,
				maxRPCAttempts: ns.numRPCAttempts,
			}
		}

		// Check ending condition.
		if ns.done() {
			ns.close()
			return
		}
	}
}

// done checks if all workers are done with their tasks and if there are are any
// tasks left to assign.
func (ns *nodeScanner) done() bool {
	// Since every work assignment sent always sends a result back (even in case
	// of failure), the main goroutine can tell if the node scan has finished by
	// checking that:
	//    - there are no assignments outstanding in workCh
	//    - there are no unprocessed results in resultCh
	//    - there are no unassigned addresses in queue
	//    - all workers are done with their assignments (totalWorkAssignments == totalResults)
	return (len(ns.workCh) == 0) && (len(ns.resultCh) == 0) && (len(ns.queue) == 0) && (ns.totalWorkAssignments == ns.totalResults)
}

// close prints out the final set of stats, adds them to the log file, and
// persists the persisted set one last time.
func (ns *nodeScanner) close() {
	fmt.Printf(ns.getStatsStr())

	// Append stats to stats file.
	json.NewEncoder(ns.scanLog).Encode(ns.stats)
	ns.scanLog.Close()

	// Save the persistData.
	ns.persistData()
}

// logWorkerResult collects the address, timestamp, and error returned
// from the scan of a single node and writes it to the scanLog as a JSON object.
// It also updates internal node scanner counters using the error returned.
func (ns *nodeScanner) logWorkerResult(res nodeScanResult) {
	err := json.NewEncoder(ns.scanLog).Encode(res)
	if err != nil {
		log.Println("Error writing nodeScanResult to file! - ", err)
	}

	if res.Err == nil {
		ns.stats.SuccessfulConnections++
		return
	}
	ns.stats.FailedConnections++

	if strings.Contains(res.Err.Error(), "unacceptable version") {
		ns.stats.UnacceptableVersionFailures++
	} else if strings.Contains(res.Err.Error(), "unreachable") {
		ns.stats.NetworkIsUnreachableFailures++
	} else if strings.Contains(res.Err.Error(), "no route to host") {
		ns.stats.NoRouteToHostFailures++
	} else if strings.Contains(res.Err.Error(), "connection refused") {
		ns.stats.ConnectionRefusedFailures++
	} else if strings.Contains(res.Err.Error(), "connection timed out") {
		ns.stats.ConnectionTimedOutFailures++
	} else if strings.Contains(res.Err.Error(), "already connected") {
		ns.stats.AlreadyConnectedFailures++
	} else {
		log.Printf("Cannot connect to local node at address %s: %s\n", res.Addr, res.Err)
	}
}

func (ns *nodeScanner) getStatsStr() string {
	s := fmt.Sprintf("Seen: %d,  Queued: %d, In WorkCh: %d, In ResultCh: %d\n", len(ns.seen), len(ns.queue), len(ns.workCh), len(ns.resultCh))
	s += fmt.Sprintf("Number assigned: %d, Number of results: %d\n", ns.totalWorkAssignments, ns.totalResults)
	s += fmt.Sprintf("Successful Connections: %d, Failed: %d\n\t(Unacceptable version: %d, Unreachable: %d, No Route: %d, Refused: %d, Timed Out: %d, Already Connected: %d)\n\n", ns.stats.SuccessfulConnections, ns.stats.FailedConnections, ns.stats.UnacceptableVersionFailures, ns.stats.NetworkIsUnreachableFailures, ns.stats.NoRouteToHostFailures, ns.stats.ConnectionRefusedFailures, ns.stats.ConnectionTimedOutFailures, ns.stats.AlreadyConnectedFailures)
	return s
}

// startWorker starts a worker that continually receives from the workCh,
// connect to the node it has been assigned, and returns all results
// using resultCh.
func startWorker(g *gateway.Gateway, workCh <-chan workAssignment, resultCh chan<- nodeScanResult) {
	for work := range workCh {
		// Try connecting to the node at this address.
		// If the connection fails, return the error message.
		err := g.Connect(work.node)
		if err != nil {
			resultCh <- nodeScanResult{
				Addr:      work.node,
				Timestamp: time.Now(),
				Err:       err,
				nodes:     nil,
			}
			continue
		}

		resultCh <- sendShareNodesRequests(g, work)
		g.Disconnect(work.node)
	}
}

const timeBetweenRequests = 50 * time.Millisecond

// Send ShareNodesRequest(s) to a node and return the set of nodes received.
func sendShareNodesRequests(g *gateway.Gateway, work workAssignment) nodeScanResult {
	result := nodeScanResult{
		Addr:      work.node,
		Err:       nil,
		Timestamp: time.Now(),
		nodes:     make(map[modules.NetAddress]struct{}),
	}

	// The ShareNodes RPC gives at most 10 random peers from the node, so
	// we repeatedly call ShareNodes in an attempt to get more peers quickly.
	for i := 0; i < work.maxRPCAttempts; i++ {
		var newNodes []modules.NetAddress
		result.Err = g.RPC(work.node, "ShareNodes", func(conn modules.PeerConn) error {
			return encoding.ReadObject(conn, &newNodes, maxSharedNodes*modules.MaxEncodedNetAddressLength)
		})
		if result.Err != nil {
			return result
		}
		for _, n := range newNodes {
			result.nodes[n] = struct{}{}
		}

		// Avoid spamming nodes by adding time between RPCs.
		time.Sleep(timeBetweenRequests)
	}

	return result
}

func (ns *nodeScanner) updateNodeStats(res nodeScanResult) {
	stats, ok := ns.data.NodeStats[res.Addr]

	// If the scan failed, and we have never persisted the node, ignore it.
	if !ok && res.Err != nil {
		return
	} else if !ok {
		// If this node isn't in the persisted set, initalize it.
		stats = nodeStats{
			FirstConnectionTime:          res.Timestamp,
			LastSuccessfulConnectionTime: res.Timestamp,
			RecentUptime:                 1,
			TotalUptime:                  1,
			UptimePercentage:             100.0,
		}
		ns.data.NodeStats[res.Addr] = stats
		return
	}

	//Update stats and uptime percentage.
	if res.Err != nil {
		stats.RecentUptime = 0
	} else {
		timeElapsed := res.Timestamp.Sub(stats.LastSuccessfulConnectionTime)
		stats.LastSuccessfulConnectionTime = res.Timestamp
		stats.RecentUptime += timeElapsed
		stats.TotalUptime += timeElapsed
	}
	// Subtract 1 from TotalUptime because we give everyone an extra second to
	// start. This makes sure the uptime rate isn't higher than 1.
	stats.UptimePercentage = 100.0 * float64(stats.TotalUptime-1) / float64(res.Timestamp.Sub(stats.FirstConnectionTime))

	ns.data.NodeStats[res.Addr] = stats
}

func (ns *nodeScanner) setupPersistFile(fileName string) error {
	ns.persistFile = fileName
	ns.data = persistData{
		StartTime: time.Now(),
		NodeStats: make(map[modules.NetAddress]nodeStats),
	}

	// Try loading the persist file.
	err := siaPersist.LoadJSON(persistMetadata, &ns.data, fileName)
	if errors.IsOSNotExist(err) {
		// Ignore the error if the file doesn't exist yet.
		// It will be created when saved for the first time.
		return nil
	}

	return err
}

func (ns *nodeScanner) persistData() error {
	return siaPersist.SaveJSON(persistMetadata, ns.data, ns.persistFile)
}
