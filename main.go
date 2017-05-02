// Copyright (c) 2017 The Decred developers
// Use of this source code is governed by an ISC
// license that can be found in the LICENSE file.

package main

import (
	"bytes"
	"flag"
	"fmt"
	"io/ioutil"
	"math"
	"net/http"
	"os"
	"os/signal"
	"sync"
	"time"

	"encoding/binary"
	"encoding/hex"
	"github.com/decred/dcrd/chaincfg"
	"github.com/decred/dcrd/wire"
	"github.com/decred/dcrrpcclient"
)

// Set some high value to check version number
var maxVersion = 10000

// Settings for daemon
var host = flag.String("host", "127.0.0.1:9109", "node RPC host:port")
var user = flag.String("user", "USER", "node RPC username")
var pass = flag.String("pass", "PASSWORD", "node RPC password")
var cert = flag.String("cert", "/home/user/.dcrd/rpc.cert", "node RPC TLS certificate (when notls=false)")
var notls = flag.Bool("notls", false, "Disable use of TLS for node connection")
var listenPort = flag.String("listen", ":8000", "web app listening port")

// Daemon Params to use
var activeNetParams = &chaincfg.MainNetParams

// Contains a certain block version's count of blocks in the
// rolling window (which has a length of activeNetParams.BlockUpgradeNumToCheck)
type blockVersions struct {
	RollingWindowLookBacks []int
}

type intervalVersionCounts struct {
	Version uint32
	Count   []uint32
}

// Set all activeNetParams fields since they don't change at runtime
var templateInformation = &templateFields{
	// BlockVersion params
	BlockVersionEnforceThreshold: int(float64(activeNetParams.BlockEnforceNumRequired) /
		float64(activeNetParams.BlockUpgradeNumToCheck) * 100),
	BlockVersionRejectThreshold: int(float64(activeNetParams.BlockRejectNumRequired) /
		float64(activeNetParams.BlockUpgradeNumToCheck) * 100),
	BlockVersionWindowLength: activeNetParams.BlockUpgradeNumToCheck,
	// StakeVersion params
	StakeVersionWindowLength: activeNetParams.StakeVersionInterval,
	StakeVersionThreshold: toFixed(float64(activeNetParams.StakeMajorityMultiplier)/
		float64(activeNetParams.StakeMajorityDivisor)*100, 0),
	// RuleChange params
	RuleChangeActivationQuorum: activeNetParams.RuleChangeActivationQuorum,
	QuorumThreshold: float64(activeNetParams.RuleChangeActivationQuorum) /
		float64(activeNetParams.RuleChangeActivationInterval*uint32(activeNetParams.TicketsPerBlock)) * 100,
}

// updatetemplateInformation is called on startup and upon every block connected notification received.
func updatetemplateInformation(dcrdClient *dcrrpcclient.Client) {
	fmt.Println("updating hard fork information")

	// Get the current best block (height and hash)
	hash, height, err := dcrdClient.GetBestBlock()
	if err != nil {
		fmt.Println(err)
		return
	}
	// Set Current block height
	templateInformation.BlockHeight = height

	// Request the current block header
	blockHeader, err := dcrdClient.GetBlockHeader(hash)
	if err != nil {
		fmt.Println(err)
		return
	}
	// Request GetStakeVersions to receive information about past block versions.
	//
	// Request twice as many, so we can populate the rolling block version window's first
	stakeVersionResults, err := dcrdClient.GetStakeVersions(hash.String(),
		int32(activeNetParams.BlockUpgradeNumToCheck*2))
	if err != nil {
		fmt.Println(err)
		return
	}
	blockVersionsFound := make(map[int32]*blockVersions)
	blockVersionsHeights := make([]int64, activeNetParams.BlockUpgradeNumToCheck)
	elementNum := 0

	// The algorithm starts at the middle of the GetStakeVersionResults and decrements backwards toward
	// the beginning of the list.  This is due to GetStakeVersionResults.StakeVersions being ordered
	// from most recent blocks to oldest. (ie [0] == current, [len] == oldest).  So by starting in the middle
	// we then can calculate that first blocks rolling window result then become one block 'more recent'
	// and calculate that blocks rolling window results.
	for i := len(stakeVersionResults.StakeVersions)/2 - 1; i >= 0; i-- {
		// Calculate the last block element in the window
		windowEnd := i + int(activeNetParams.BlockUpgradeNumToCheck)
		// blockVersionsHeights lets us have a correctly ordered list of blockheights for xaxis label
		blockVersionsHeights[elementNum] = stakeVersionResults.StakeVersions[i].Height
		// Define rolling window range for this current block (i)
		stakeVersionsWindow := stakeVersionResults.StakeVersions[i:windowEnd]
		for _, stakeVersion := range stakeVersionsWindow {
			// Try to get an existing blockVersions struct (pointer)
			theseBlockVersions, ok := blockVersionsFound[stakeVersion.BlockVersion]
			if !ok {
				// Had not found this block version yet
				theseBlockVersions = &blockVersions{}
				blockVersionsFound[stakeVersion.BlockVersion] = theseBlockVersions
				theseBlockVersions.RollingWindowLookBacks =
					make([]int, activeNetParams.BlockUpgradeNumToCheck)
				// Need to populate "back" to fill in values for previously missed window
				for k := 0; k < elementNum; k++ {
					theseBlockVersions.RollingWindowLookBacks[k] = 0
				}
				theseBlockVersions.RollingWindowLookBacks[elementNum] = 1
			} else {
				// Already had that block version, so increment
				theseBlockVersions.RollingWindowLookBacks[elementNum]++
			}
		}
		elementNum++
	}
	templateInformation.BlockVersionsHeights = blockVersionsHeights
	templateInformation.BlockVersions = blockVersionsFound

	// Pick min block version (current version) out of most recent window
	stakeVersionsWindow := stakeVersionResults.StakeVersions[:activeNetParams.BlockUpgradeNumToCheck]
	blockVersionsCounts := make(map[int32]int64)
	for _, sv := range stakeVersionsWindow {
		blockVersionsCounts[sv.BlockVersion] = blockVersionsCounts[sv.BlockVersion] + 1
	}
	var minBlockVersion, maxBlockVersion, popBlockVersion int32 = math.MaxInt32, -1, 0
	for v := range blockVersionsCounts {
		if v < minBlockVersion {
			minBlockVersion = v
		}
		if v > maxBlockVersion {
			maxBlockVersion = v
		}
	}
	popBlockVersionCount := int64(-1)
	for v, c := range blockVersionsCounts {
		if c > popBlockVersionCount && v != minBlockVersion {
			popBlockVersionCount = c
			popBlockVersion = v
		}
	}

	blockWinUpgradePct := func(count int64) float64 {
		return 100 * float64(count) / float64(activeNetParams.BlockUpgradeNumToCheck)
	}

	templateInformation.BlockVersionCurrent = minBlockVersion

	templateInformation.BlockVersionMostPopular = popBlockVersion
	templateInformation.BlockVersionMostPopularPercentage = toFixed(blockWinUpgradePct(popBlockVersionCount), 2)

	templateInformation.BlockVersionNext = minBlockVersion + 1
	templateInformation.BlockVersionNextPercentage = toFixed(blockWinUpgradePct(blockVersionsCounts[minBlockVersion+1]), 2)

	if popBlockVersionCount > int64(activeNetParams.BlockEnforceNumRequired) {
		templateInformation.BlockVersionSuccess = true
	}

	// Voting intervals ((height-4096) mod 2016)
	blocksIntoStakeVersionInterval := (height - activeNetParams.StakeValidationHeight) %
		activeNetParams.StakeVersionInterval
	// Stake versions per block in current voting interval (getstakeversions hash blocksIntoInterval)
	intervalStakeVersions, err := dcrdClient.GetStakeVersions(hash.String(),
		int32(blocksIntoStakeVersionInterval))
	if err != nil {
		fmt.Println(err)
	}
	// Tally missed votes so far in this interval
	missedVotesStakeInterval := 0
	for _, stakeVersionResult := range intervalStakeVersions.StakeVersions {
		missedVotesStakeInterval += int(activeNetParams.TicketsPerBlock) - len(stakeVersionResult.Votes)
	}

	// Vote tallies for previous intervals (getstakeversioninfo 4)
	numberOfIntervalsToCheck := 4
	stakeVersionInfo, err := dcrdClient.GetStakeVersionInfo(int32(numberOfIntervalsToCheck))
	if err != nil {
		fmt.Println(err)
		return
	}
	numIntervals := len(stakeVersionInfo.Intervals)
	if numIntervals == 0 {
		fmt.Println("StakeVersion info did not return usable information, intervals empty")
		return
	}
	templateInformation.StakeVersionsIntervals = stakeVersionInfo.Intervals

	minimumNeededVoteVersions := uint32(100)
	// Hacky way of populating the Vote Version bar graph
	// Each element in each dataset needs counts for each interval
	// For example:
	// version 1: [100, 200, 0, 400]
	var stakeVersionIntervalResults []intervalVersionCounts
	stakeVersionLabels := make([]string, numIntervals)
	// Oldest to newest interval (charts are left to right)
	for i := 0; i < numIntervals; i++ {
		interval := &stakeVersionInfo.Intervals[numIntervals-1-i]
		stakeVersionLabels[i] = fmt.Sprintf("%v - %v", interval.StartHeight, interval.EndHeight)
	versionloop:
		for _, versionCount := range interval.VoteVersions {
			// Is this a vote version we've seen in a previous interval?
			for k, result := range stakeVersionIntervalResults {
				if result.Version == versionCount.Version {
					stakeVersionIntervalResults[k].Count[i] = versionCount.Count
					continue versionloop
				}
			}
			if versionCount.Count > minimumNeededVoteVersions {
				stakeVersionIntervalResult := intervalVersionCounts{
					Version: versionCount.Version,
					Count:   make([]uint32, numIntervals),
				}
				stakeVersionIntervalResult.Count[i] = versionCount.Count
				stakeVersionIntervalResults = append(stakeVersionIntervalResults, stakeVersionIntervalResult)
			}
		}
	}
	stakeVersionLabels[numIntervals-1] = "Current Interval"
	currentInterval := stakeVersionInfo.Intervals[0]

	maxPossibleVotes := activeNetParams.StakeVersionInterval*int64(activeNetParams.TicketsPerBlock) -
		int64(missedVotesStakeInterval)

	templateInformation.StakeVersionIntervalResults = stakeVersionIntervalResults
	templateInformation.StakeVersionWindowVoteTotal = maxPossibleVotes
	templateInformation.StakeVersionIntervalLabels = stakeVersionLabels
	templateInformation.StakeVersionCurrent = blockHeader.StakeVersion

	var mostPopularVersion, mostPopularVersionCount uint32
	for _, stakeVersion := range currentInterval.VoteVersions {
		if stakeVersion.Version > blockHeader.StakeVersion &&
			stakeVersion.Count > mostPopularVersionCount {
			mostPopularVersion = stakeVersion.Version
			mostPopularVersionCount = stakeVersion.Count
		}
	}

	templateInformation.StakeVersionMostPopularCount = mostPopularVersionCount
	templateInformation.StakeVersionMostPopularPercentage = toFixed(float64(mostPopularVersionCount)/
		float64(maxPossibleVotes)*100, 2)
	templateInformation.StakeVersionMostPopular = mostPopularVersion
	templateInformation.StakeVersionRequiredVotes = int32(maxPossibleVotes) *
		activeNetParams.StakeMajorityMultiplier / activeNetParams.StakeMajorityDivisor
	if int32(mostPopularVersionCount) > templateInformation.StakeVersionRequiredVotes {
		templateInformation.StakeVersionSuccess = true
	}

	blocksIntoInterval := currentInterval.EndHeight - currentInterval.StartHeight
	templateInformation.StakeVersionVotesRemaining =
		(activeNetParams.StakeVersionInterval - blocksIntoInterval) * int64(activeNetParams.TicketsPerBlock)

	// Quorum/vote information
	getVoteInfo, err := dcrdClient.GetVoteInfo(mostPopularVersion)
	if err != nil {
		fmt.Println("Get vote info err", err)
		templateInformation.Quorum = false
		return
	}
	templateInformation.GetVoteInfoResult = getVoteInfo

	// There may be no agendas for this vote version
	if len(getVoteInfo.Agendas) == 0 {
		fmt.Printf("No agendas for vote version %d\n", mostPopularVersion)
		templateInformation.Agendas = []Agenda{}
		return
	}

	// Set Quorum to true since we got a valid response back from GetVoteInfoResult (?)
	if getVoteInfo.TotalVotes >= getVoteInfo.Quorum {
		templateInformation.Quorum = true
	}

	// Status LockedIn Circle3 Ring Indicates BlocksLeft until old versions gets denied
	lockedinBlocksleft := float64(getVoteInfo.EndHeight) - float64(getVoteInfo.CurrentHeight)
	lockedinWindowsize := float64(getVoteInfo.EndHeight) - float64(getVoteInfo.StartHeight)
	lockedinPercentage := lockedinWindowsize / 100

	templateInformation.LockedinPercentage = toFixed(lockedinBlocksleft/lockedinPercentage, 2)
	templateInformation.Agendas = make([]Agenda, 0, len(getVoteInfo.Agendas))

	for i := range getVoteInfo.Agendas {
		choiceIds := make([]string, len(getVoteInfo.Agendas[i].Choices))
		choicePercentages := make([]float64, len(getVoteInfo.Agendas[i].Choices))
		for i, choice := range getVoteInfo.Agendas[i].Choices {
			if !choice.IsAbstain {
				choiceIds[i] = choice.Id
				choicePercentages[i] = toFixed(choice.Progress*100, 2)
			}
		}

		templateInformation.Agendas = append(templateInformation.Agendas, Agenda{
			Agenda:                    getVoteInfo.Agendas[i],
			QuorumExpirationDate:      time.Unix(int64(getVoteInfo.Agendas[i].ExpireTime), int64(0)).Format(time.RFC850),
			QuorumVotedPercentage:     toFixed(getVoteInfo.Agendas[i].QuorumProgress*100, 2),
			QuorumAbstainedPercentage: toFixed(getVoteInfo.Agendas[i].Choices[0].Progress*100, 2),
			ChoiceIDs:                 choiceIds,
			ChoicePercentages:         choicePercentages,
			StartHeight:               getVoteInfo.StartHeight,
		})
	}
}

// main wraps mainCore, which does all the work, because deferred functions do
/// not run after os.Exit().
func main() {
	os.Exit(mainCore())
}

func mainCore() int {
	flag.Parse()

	// Chans for rpccclient notification handlers
	connectChan := make(chan int64, 100)
	quit := make(chan struct{})

	// Read in current dcrd cert
	var dcrdCerts []byte
	var err error
	if !*notls {
		dcrdCerts, err = ioutil.ReadFile(*cert)
		if err != nil {
			fmt.Printf("Failed to read dcrd cert file at %s: %s\n", *cert,
				err.Error())
			return 1
		}
	}

	// Set up notification handler that will release ntfns when new blocks connect
	ntfnHandlersDaemon := dcrrpcclient.NotificationHandlers{
		OnBlockConnected: func(serializedBlockHeader []byte, transactions [][]byte) {
			var blockHeader wire.BlockHeader
			errLocal := blockHeader.Deserialize(bytes.NewReader(serializedBlockHeader))
			if errLocal != nil {
				fmt.Printf("Failed to deserialize block header: %v\n", errLocal.Error())
				return
			}
			fmt.Println("got a new block passing it", blockHeader.Height)
			connectChan <- int64(blockHeader.Height)
		},
	}

	// dcrrpclient configuration
	connCfgDaemon := &dcrrpcclient.ConnConfig{
		Host:         *host,
		Endpoint:     "ws",
		User:         *user,
		Pass:         *pass,
		Certificates: dcrdCerts,
		DisableTLS:   *notls,
	}

	fmt.Printf("Attempting to connect to dcrd RPC %s as user %s "+
		"using certificate located in %s\n", *host, *user, *cert)
	// Attempt to connect rpcclient and daemon
	dcrdClient, err := dcrrpcclient.New(connCfgDaemon, &ntfnHandlersDaemon)
	if err != nil {
		fmt.Printf("Failed to start dcrd rpcclient: %s\n", err.Error())
		return 1
	}
	defer func() {
		fmt.Printf("Disconnecting from dcrd.\n")
		dcrdClient.Disconnect()
	}()

	// Subscribe to block notifications
	if err = dcrdClient.NotifyBlocks(); err != nil {
		fmt.Printf("Failed to start register daemon rpc client for  "+
			"block notifications: %s\n", err.Error())
		return 1
	}

	// Only accept a single CTRL+C
	c := make(chan os.Signal, 1)
	signal.Notify(c, os.Interrupt)

	// Start waiting for the interrupt signal
	go func() {
		<-c
		signal.Stop(c)
		// Close the channel so multiple goroutines can get the message
		fmt.Println("CTRL+C hit.  Closing.")
		close(quit)
		return
	}()

	// Run an initial templateInforation update based on current change
	updatetemplateInformation(dcrdClient)

	// Run goroutine for notifications
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		for {
			select {
			case height := <-connectChan:
				fmt.Printf("Block height %v connected\n", height)
				updatetemplateInformation(dcrdClient)
			case <-quit:
				fmt.Printf("Closing hardfork demo.\n")
				wg.Done()
				return
			}
		}
	}()

	// Create new web UI to deal with HTML templates and provide the
	// http.HandleFunc for the web server
	webUI := NewWebUI()
	webUI.TemplateData = templateInformation
	// Register OS signal (USR1 on non-Windows platforms) to reload templates
	webUI.UseSIGToReloadTemplates()

	// URL handlers for js/css/fonts/images
	http.HandleFunc("/", webUI.demoPage)
	http.Handle("/js/", http.StripPrefix("/js/", http.FileServer(http.Dir("public/js/"))))
	http.Handle("/css/", http.StripPrefix("/css/", http.FileServer(http.Dir("public/css/"))))
	http.Handle("/fonts/", http.StripPrefix("/fonts/", http.FileServer(http.Dir("public/fonts/"))))
	http.Handle("/images/", http.StripPrefix("/images/", http.FileServer(http.Dir("public/images/"))))

	// Start http server listening and serving, but no way to signal to quit
	go func() {
		err = http.ListenAndServe(*listenPort, nil)
		if err != nil {
			fmt.Printf("Failed to bind http server: %s\n", err.Error())
			close(quit)
		}
	}()

	// Wait for goroutines, such as the block connected handler loop
	wg.Wait()

	return 0
}

// Some various helper math helper funcs
func round(num float64) int {
	return int(num + math.Copysign(0.5, num))
}

func toFixed(num float64, precision int) float64 {
	output := math.Pow(10, float64(precision))
	return float64(round(num*output)) / output
}

func getBlockVersionFromWork(dcrdClient *dcrrpcclient.Client) (uint32, error) {
	getWorkResult, err := dcrdClient.GetWork()
	if err != nil {
		return 0, err
	}
	blockVerBytes, _ := hex.DecodeString(getWorkResult.Data[0:8])
	return binary.LittleEndian.Uint32(blockVerBytes), nil
}
