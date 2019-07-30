package renter

import (
	"fmt"
	"io/ioutil"
	"sync"
	"time"

	"gitlab.com/NebulousLabs/errors"
	"gitlab.com/NebulousLabs/fastrand"

	"gitlab.com/NebulousLabs/Sia/build"
	"gitlab.com/NebulousLabs/Sia/modules"
)

var (
	// errNoStuckFiles is a helper to indicate that there are no stuck files in
	// the renter's directory
	errNoStuckFiles = errors.New("no stuck files")

	// errNoStuckChunks is a helper to indicate that there are no stuck chunks
	// in a siafile
	errNoStuckChunks = errors.New("no stuck chunks")
)

type (
	// stuckQueue contains a FIFO queue of files that have had a stuck chunk
	// successfully repaired
	stuckQueue struct {
		queue    []modules.SiaPath
		siaPaths map[modules.SiaPath]struct{}

		mu sync.Mutex
	}
)

// managedLen returns the length of the queue
func (sq *stuckQueue) managedLen() int {
	sq.mu.Lock()
	defer sq.mu.Unlock()
	return len(sq.queue)
}

// managedPop returns the first element in the queue
func (sq *stuckQueue) managedPop() (sp modules.SiaPath) {
	sq.mu.Lock()
	defer sq.mu.Unlock()

	sp, sq.queue = sq.queue[0], sq.queue[1:]
	delete(sq.siaPaths, sp)
	return
}

// managedPush tries to add a file to the queue
func (sq *stuckQueue) managedPush(siaPath modules.SiaPath) {
	sq.mu.Lock()
	defer sq.mu.Unlock()

	// Check if there is room in the queue
	if len(sq.queue) >= maxSuccessfulStuckRepairFiles {
		return
	}

	// Check if the file is already being tracked
	if _, ok := sq.siaPaths[siaPath]; ok {
		return
	}

	// Add file to the queue
	sq.queue = append(sq.queue, siaPath)
	sq.siaPaths[siaPath] = struct{}{}
	return
}

// managedAddStuckChunksToHeap tries to add as many stuck chunks from a siafile
// to the upload heap as possible
func (r *Renter) managedAddStuckChunksToHeap(siaPath modules.SiaPath) error {
	// Open File
	sf, err := r.staticFileSet.Open(siaPath)
	if err != nil {
		return fmt.Errorf("unable to open siafile %v, error: %v", siaPath, err)
	}
	defer sf.Close()

	// Check if there are still stuck chunks to repair
	if sf.NumStuckChunks() == 0 {
		return errNoStuckChunks
	}

	// Build unfinished stuck chunks
	hosts := r.managedRefreshHostsAndWorkers()
	offline, goodForRenew, _ := r.managedContractUtilityMaps()
	unfinishedStuckChunks := r.managedBuildUnfinishedChunks(sf, hosts, targetStuckChunks, offline, goodForRenew)

	// Add up to maxStuckChunksInHeap stuck chunks to the upload heap
	var chunk *unfinishedUploadChunk
	stuckChunksAdded := 0
	for len(unfinishedStuckChunks) < 0 && stuckChunksAdded < maxStuckChunksInHeap {
		chunk, unfinishedStuckChunks = unfinishedStuckChunks[0], unfinishedStuckChunks[1:]
		chunk.stuckRepair = true
		if !r.uploadHeap.managedPush(chunk) {
			// Stuck chunk unable to be added. Close the file entry of that
			// chunk
			if err = chunk.fileEntry.Close(); err != nil {
				r.log.Println("WARN: unable to close file:", err)
			}
			continue
		}
		stuckChunksAdded++
	}

	// check if there are more stuck chunks in the file
	if len(unfinishedStuckChunks) == 0 {
		return nil
	}

	// Since there are more stuck chunks in the file try and add it back to the
	// queue
	//
	// NOTE: currently not re-prioritizing this file. I believe this is OK since
	// it helps the stuck loop move on to other files. If we want to keep
	// prioritizing this file until all the stuck chunks have been added then we
	// can change this line.
	r.stuckQueue.managedPush(siaPath)

	// Close out remaining file entries
	for _, chunk := range unfinishedStuckChunks {
		if err = chunk.fileEntry.Close(); err != nil {
			r.log.Println("WARN: unable to close file:", err)
		}
	}
	return nil
}

// managedOldestHealthCheckTime finds the lowest level directory with the oldest
// LastHealthCheckTime
func (r *Renter) managedOldestHealthCheckTime() (modules.SiaPath, time.Time, error) {
	// Check the siadir metadata for the root files directory
	siaPath := modules.RootSiaPath()
	metadata, err := r.managedDirectoryMetadata(siaPath)
	if err != nil {
		return modules.SiaPath{}, time.Time{}, err
	}

	// Follow the path of oldest LastHealthCheckTime to the lowest level
	// directory
	for metadata.NumSubDirs > 0 {
		// Check to make sure renter hasn't been shutdown
		select {
		case <-r.tg.StopChan():
			return modules.SiaPath{}, time.Time{}, errors.New("Renter shutdown before oldestHealthCheckTime could be found")
		default:
		}

		// Check for sub directories
		subDirSiaPaths, err := r.managedSubDirectories(siaPath)
		if err != nil {
			return modules.SiaPath{}, time.Time{}, err
		}

		// Find the oldest LastHealthCheckTime of the sub directories
		updated := false
		for _, subDirPath := range subDirSiaPaths {
			// Check to make sure renter hasn't been shutdown
			select {
			case <-r.tg.StopChan():
				return modules.SiaPath{}, time.Time{}, errors.New("Renter shutdown before oldestHealthCheckTime could be found")
			default:
			}

			// Check lastHealthCheckTime of sub directory
			subMetadata, err := r.managedDirectoryMetadata(subDirPath)
			if err != nil {
				return modules.SiaPath{}, time.Time{}, err
			}

			// If the LastHealthCheckTime is after current LastHealthCheckTime
			// continue since we are already in a directory with an older
			// timestamp
			if subMetadata.AggregateLastHealthCheckTime.After(metadata.AggregateLastHealthCheckTime) {
				continue
			}

			// Update LastHealthCheckTime and follow older path
			updated = true
			metadata = subMetadata
			siaPath = subDirPath
		}

		// If the values were never updated with any of the sub directory values
		// then return as we are in the directory we are looking for
		if !updated {
			return siaPath, metadata.AggregateLastHealthCheckTime, nil
		}
	}

	return siaPath, metadata.AggregateLastHealthCheckTime, nil
}

// managedStuckDirectory randomly finds a directory that contains stuck chunks
func (r *Renter) managedStuckDirectory() (modules.SiaPath, error) {
	// Iterating of the renter directory until randomly ending up in a
	// directory, break and return that directory
	siaPath := modules.RootSiaPath()
	for {
		select {
		// Check to make sure renter hasn't been shutdown
		case <-r.tg.StopChan():
			return modules.SiaPath{}, nil
		default:
		}

		directories, err := r.DirList(siaPath)
		if err != nil {
			return modules.SiaPath{}, err
		}
		files, err := r.FileList(siaPath, false, false)
		if err != nil {
			return modules.SiaPath{}, err
		}
		// Sanity check that there is at least the current directory
		if len(directories) == 0 {
			build.Critical("No directories returned from DirList")
		}
		// Check if we are in an empty Directory. This will be the case before
		// any files have been uploaded so the root directory is empty. Also it
		// could happen if the only file in a directory was stuck and was very
		// recently deleted so the health of the directory has not yet been
		// updated.
		emptyDir := len(directories) == 1 && len(files) == 0
		if emptyDir {
			return siaPath, errNoStuckFiles
		}
		// Check if there are stuck chunks in this directory
		if directories[0].AggregateNumStuckChunks == 0 {
			// Log error if we are not at the root directory
			if !siaPath.IsRoot() {
				r.log.Debugln("WARN: ended up in directory with no stuck chunks that is not root directory:", siaPath)
			}
			return siaPath, errNoStuckFiles
		}
		// Check if we have reached a directory with only files
		if len(directories) == 1 {
			return siaPath, nil
		}

		// Get random int
		rand := fastrand.Intn(int(directories[0].AggregateNumStuckChunks))

		// Use rand to decide which directory to go into. Work backwards over
		// the slice of directories. Since the first element is the current
		// directory that means that it is the sum of all the files and
		// directories.  We can chose a directory by subtracting the number of
		// stuck chunks a directory has from rand and if rand gets to 0 or less
		// we choose that directory
		for i := len(directories) - 1; i >= 0; i-- {
			// If we make it to the last iteration double check that the current
			// directory has files
			if i == 0 && len(files) == 0 {
				break
			}

			// If we are on the last iteration and the directory does have files
			// then return the current directory
			if i == 0 {
				siaPath = directories[0].SiaPath
				return siaPath, nil
			}

			// Skip directories with no stuck chunks
			if directories[i].AggregateNumStuckChunks == uint64(0) {
				continue
			}

			rand = rand - int(directories[i].AggregateNumStuckChunks)
			siaPath = directories[i].SiaPath
			// If rand is less than 0 break out of the loop and continue into
			// that directory
			if rand <= 0 {
				break
			}
		}
	}
}

// managedSubDirectories reads a directory and returns a slice of all the sub
// directory SiaPaths
func (r *Renter) managedSubDirectories(siaPath modules.SiaPath) ([]modules.SiaPath, error) {
	// Read directory
	fileinfos, err := ioutil.ReadDir(siaPath.SiaDirSysPath(r.staticFilesDir))
	if err != nil {
		return nil, err
	}
	// Find all sub directory SiaPaths
	folders := make([]modules.SiaPath, 0, len(fileinfos))
	for _, fi := range fileinfos {
		if fi.IsDir() {
			subDir, err := siaPath.Join(fi.Name())
			if err != nil {
				return nil, err
			}
			folders = append(folders, subDir)
		}
	}
	return folders, nil
}

// threadedStuckFileLoop works through the renter directory and finds the stuck
// chunks and tries to repair them
func (r *Renter) threadedStuckFileLoop() {
	err := r.tg.Add()
	if err != nil {
		return
	}
	defer r.tg.Done()

	// Loop until the renter has shutdown or until there are no stuck chunks
	for {
		// Return if the renter has shut down.
		select {
		case <-r.tg.StopChan():
			return
		default:
		}

		// Wait until the renter is online to proceed.
		if !r.managedBlockUntilOnline() {
			// The renter shut down before the internet connection was restored.
			r.log.Debugln("renter shutdown before internet connection")
			return
		}

		// As we add stuck chunks to the upload heap we want to remember the
		// directories they came from so we can call bubble to update the
		// filesystem
		var dirSiaPaths []modules.SiaPath

		// Try and add any stuck chunks from files that previously had a
		// successful stuck chunk in FIFO order. We add these chunks first since
		// the previous success gives us more confidence that it is more likely
		// additional stuck chunks from these files will be successful compared
		// to a random stuck chunk from the renter's directory.
		for r.stuckQueue.managedLen() > 0 && r.uploadHeap.managedNumStuckChunks() < maxStuckChunksInHeap {
			// Pop the first file SiaPath
			siaPath := r.stuckQueue.managedPop()

			// Add stuck chunks to uploadHeap
			err = r.managedAddStuckChunksToHeap(siaPath)
			if err != nil && err != errNoStuckChunks {
				r.log.Println("WARN: error adding stuck chunks to heap:", err)
			}
			// If there are no longer stuck chunks in the file, continue to the
			// next file
			if err == errNoStuckChunks {
				continue
			}
			// Since we either added stuck chunks to the heap from this file or
			// all the stuck chunks for the file are already being worked on,
			// remember the directory so we can call bubble on it at the end of
			// this iteration of the stuck loop to update the filesystem
			dirSiaPath, err := siaPath.Dir()
			if err != nil {
				r.log.Println("WARN: error getting directory siapath:", err)
				continue
			}
			dirSiaPaths = append(dirSiaPaths, dirSiaPath)
		}

		// Check if there is room in the uploadHeap for more stuck chunks
		prevNumStuckChunks := r.uploadHeap.managedNumStuckChunks()
		for r.uploadHeap.managedNumStuckChunks() < maxStuckChunksInHeap {
			// Randomly get directory with stuck files
			dirSiaPath, err := r.managedStuckDirectory()
			if err != nil {
				// If there was an error, log the error and break out of the
				// loop. There are either stuck chunks to work on or the loop
				// will sleep until there is more work to do. In both cases
				// there is protection against rapid cycling so there is no need
				// to sleep here
				r.log.Debugln("WARN: error getting random stuck directory:", err)
				break
			}
			// Remember the directory so bubble can be called on it at the end
			// of the iteration
			dirSiaPaths = append(dirSiaPaths, dirSiaPath)

			// Refresh the worker pool and get the set of hosts that are
			// currently useful for uploading.
			hosts := r.managedRefreshHostsAndWorkers()

			// Add stuck chunks to upload heap and signal repair needed
			r.managedBuildChunkHeap(dirSiaPath, hosts, targetStuckChunks)

			// Sanity check that stuck chunks were added
			currentNumStuckChunks := r.uploadHeap.managedNumStuckChunks()
			if currentNumStuckChunks <= prevNumStuckChunks {
				// If the number of stuck chunks in the heap is not increasing
				// then break out of this loop in order to prevent getting stuck
				// in an infinite loop
				break
			}
			r.log.Debugf("Attempting to repair %v stuck chunks from directory `%s`", currentNumStuckChunks-prevNumStuckChunks, dirSiaPath.String())
			prevNumStuckChunks = currentNumStuckChunks
		}

		// Check if any stuck chunks were added to the upload heap
		if r.uploadHeap.managedNumStuckChunks() == 0 {
			// Block until new work is required.
			select {
			case <-r.tg.StopChan():
				// The renter has shut down.
				return
			case <-r.uploadHeap.stuckChunkFound:
				// Health Loop found stuck chunk
			case <-r.uploadHeap.stuckChunkSuccess:
				// Stuck chunk was successfully repaired.
			}
			continue
		}

		// Signal that a repair is needed because stuck chunks were added to the
		// upload heap
		select {
		case r.uploadHeap.repairNeeded <- struct{}{}:
		default:
		}

		// Sleep until it is time to try and repair another stuck chunk
		rebuildStuckHeapSignal := time.After(repairStuckChunkInterval)
		select {
		case <-r.tg.StopChan():
			// Return if the return has been shutdown
			return
		case <-rebuildStuckHeapSignal:
			// Time to find another random chunk
		case <-r.uploadHeap.stuckChunkSuccess:
			// Stuck chunk was successfully repaired.
		}

		// Call bubble before continuing on next iteration to ensure filesystem
		// is updated.
		for _, dirSiaPath := range dirSiaPaths {
			err = r.managedBubbleMetadata(dirSiaPath)
			if err != nil {
				r.log.Println("Error calling managedBubbleMetadata on `", dirSiaPath.String(), "`:", err)
				select {
				case <-time.After(stuckLoopErrorSleepDuration):
				case <-r.tg.StopChan():
					return
				}
			}
		}
	}
}

// threadedUpdateRenterHealth reads all the siafiles in the renter, calculates
// the health of each file and updates the folder metadata
func (r *Renter) threadedUpdateRenterHealth() {
	err := r.tg.Add()
	if err != nil {
		return
	}
	defer r.tg.Done()

	// Loop until the renter has shutdown or until the renter's top level files
	// directory has a LasHealthCheckTime within the healthCheckInterval
	for {
		select {
		// Check to make sure renter hasn't been shutdown
		case <-r.tg.StopChan():
			return
		default:
		}

		// Follow path of oldest time, return directory and timestamp
		r.log.Debugln("Checking for oldest health check time")
		siaPath, lastHealthCheckTime, err := r.managedOldestHealthCheckTime()
		if err != nil {
			// If there is an error getting the lastHealthCheckTime sleep for a
			// little bit before continuing
			r.log.Debug("WARN: Could not find oldest health check time:", err)
			select {
			case <-time.After(healthLoopErrorSleepDuration):
			case <-r.tg.StopChan():
				return
			}
			continue
		}

		// Check if the time since the last check on the least recently checked
		// folder is inside the health check interval. If so, the whole
		// filesystem has been checked recently, and we can sleep until the
		// least recent check is outside the check interval.
		timeSinceLastCheck := time.Since(lastHealthCheckTime)
		if timeSinceLastCheck < healthCheckInterval {
			// Sleep until the least recent check is outside the check interval.
			sleepDuration := healthCheckInterval - timeSinceLastCheck
			r.log.Debugln("Health loop sleeping for", sleepDuration)
			wakeSignal := time.After(sleepDuration)
			select {
			case <-r.tg.StopChan():
				return
			case <-wakeSignal:
			}
		}
		r.log.Debug("Health Loop calling bubble on '", siaPath.String(), "'")
		err = r.managedBubbleMetadata(siaPath)
		if err != nil {
			r.log.Println("Error calling managedBubbleMetadata on `", siaPath.String(), "`:", err)
			select {
			case <-time.After(healthLoopErrorSleepDuration):
			case <-r.tg.StopChan():
				return
			}
		}
	}
}
