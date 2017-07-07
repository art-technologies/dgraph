/*
 * Copyright (C) 2017 Dgraph Labs, Inc. and Contributors
 *
 * This program is free software: you can redistribute it and/or modify
 * it under the terms of the GNU Affero General Public License as published by
 * the Free Software Foundation, either version 3 of the License, or
 * (at your option) any later version.
 *
 * This program is distributed in the hope that it will be useful,
 * but WITHOUT ANY WARRANTY; without even the implied warranty of
 * MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
 * GNU Affero General Public License for more details.
 *
 * You should have received a copy of the GNU Affero General Public License
 * along with this program.  If not, see <http://www.gnu.org/licenses/>.
 */

package posting

import (
	"context"
	"crypto/md5"
	"flag"
	"fmt"
	"log"
	"math"
	"math/rand"
	"runtime"
	"runtime/debug"
	"sync"
	"time"

	"golang.org/x/net/trace"

	"github.com/dgraph-io/badger"
	"github.com/dgryski/go-farm"

	"github.com/dgraph-io/dgraph/protos"
	"github.com/dgraph-io/dgraph/x"
)

var (
	maxmemory = flag.Float64("stw_ram_mb", 4096.0,
		"If RAM usage exceeds this, we stop the world, and flush our buffers.")

	commitFraction   = flag.Float64("gentlecommit", 0.10, "Fraction of dirty posting lists to commit every few seconds.")
	lhmapNumShards   = runtime.NumCPU() * 4
	dummyPostingList []byte // Used for indexing.
)

const (
	MB = 1 << 20
)

// syncMarks stores the watermark for synced RAFT proposals. Each RAFT proposal consists
// of many individual mutations, which could be applied to many different posting lists.
// Thus, each PL when being mutated would send an undone Mark, and each list would
// accumulate all such pending marks. When the PL is synced to RocksDB, it would
// mark all the pending ones as done.
// This ideally belongs to RAFT node struct (where committed watermark is being tracked),
// but because the logic of mutations is
// present here and to avoid a circular dependency, we've placed it here.
// Note that there's one watermark for each RAFT node/group.
// This watermark would be used for taking snapshots, to ensure that all the data and
// index mutations have been syned to RocksDB, before a snapshot is taken, and previous
// RAFT entries discarded.
type syncMarks struct {
	sync.RWMutex
	m map[uint32]*x.WaterMark
}

func init() {
	x.AddInit(func() {
		h := md5.New()
		pl := protos.PostingList{
			Checksum: h.Sum(nil),
		}
		var err error
		dummyPostingList, err = pl.Marshal()
		x.Check(err)
	})
}

func (g *syncMarks) create(group uint32) *x.WaterMark {
	g.Lock()
	defer g.Unlock()
	if g.m == nil {
		g.m = make(map[uint32]*x.WaterMark)
	}

	if prev, present := g.m[group]; present {
		return prev
	}
	w := &x.WaterMark{Name: fmt.Sprintf("Synced: Group %d", group)}
	w.Init()
	g.m[group] = w
	return w
}

func (g *syncMarks) Get(group uint32) *x.WaterMark {
	g.RLock()
	if w, present := g.m[group]; present {
		g.RUnlock()
		return w
	}
	g.RUnlock()
	return g.create(group)
}

// SyncMarkFor returns the synced watermark for the given RAFT group.
// We use this to determine the index to use when creating a new snapshot.
func SyncMarkFor(group uint32) *x.WaterMark {
	return marks.Get(group)
}

type listMaps struct {
	x.SafeMutex
	m map[uint32]*listMap
}

func (l *listMaps) create(group uint32) *listMap {
	l.Lock()
	defer l.Unlock()
	if l.m == nil {
		l.m = make(map[uint32]*listMap)
	}

	if prev, present := l.m[group]; present {
		return prev
	}
	lhmap := newShardedListMap(lhmapNumShards)
	l.m[group] = lhmap
	return lhmap
}

func (l *listMaps) get(group uint32) *listMap {
	// Don't store anything in group zero since we never compact
	// group 0 logs
	x.AssertTruef(group != 0, "group id is 0 for lhmap")
	l.RLock()
	if lhmap, present := l.m[group]; present {
		l.RUnlock()
		return lhmap
	}
	l.RUnlock()
	return l.create(group)
}

func (l *listMaps) groups() []uint32 {
	l.RLock()
	defer l.RUnlock()
	var groups []uint32
	for k := range l.m {
		groups = append(groups, k)
	}
	return groups
}

func lhmapFor(group uint32) *listMap {
	return lhmaps.get(group)
}

func gentleCommit(dirtyMap map[fingerPrint]time.Time, pending chan struct{},
	commitFraction float64) {
	select {
	case pending <- struct{}{}:
	default:
		fmt.Printf("Skipping gentleCommit len(syncCh) %v,\n", len(syncCh))
		return
	}

	// NOTE: No need to acquire read lock for stopTheWorld. This portion is being run
	// serially alongside aggressive commit.
	n := int(float64(len(dirtyMap)) * commitFraction)
	if n < 1000 {
		// Have a min value of n, so we can merge small number of dirty PLs fast.
		n = 1000
	}
	keysBuffer := make([]fingerPrint, 0, n)

	// Convert map to list.
	var loops int
	for key, ts := range dirtyMap {
		loops++
		if loops > 3*n {
			break
		}
		if time.Since(ts) < 5*time.Second {
			continue
		}

		delete(dirtyMap, key)
		keysBuffer = append(keysBuffer, key)
		if len(keysBuffer) >= n {
			// We don't want to process the entire dirtyMap in one go.
			break
		}
	}

	go func(keys []fingerPrint) {
		defer func() { <-pending }()
		if len(keys) == 0 {
			return
		}
		for _, key := range keys {
			l := lhmapFor(key.gid).Get(key.fp)
			if l == nil {
				continue
			}
			// Not removing the postings list from the map, to avoid a race condition,
			// where another caller re-creates the posting list before a commit happens.
			commitOne(l)
		}
	}(keysBuffer)
}

func periodicFree() {
	ticker := time.NewTicker(5 * time.Second)
	for range ticker.C {
		var ms runtime.MemStats
		runtime.ReadMemStats(&ms)

		megs := (ms.HeapInuse + ms.StackInuse) / MB
		inUse := float64(megs)
		idle := float64((ms.HeapIdle - ms.HeapReleased) / MB)

		if inUse+idle > *maxmemory {
			fmt.Printf("Inuse: %.0f idle: %.0f. Freeing OS memory", inUse, idle)
			x.UpdateMemoryStatus(false)
			debug.FreeOSMemory()
		} else {
			x.UpdateMemoryStatus(true)
		}
	}
}

// periodicMerging periodically merges the dirty posting lists. It also checks our memory
// usage. If it exceeds a certain threshold, it would stop the world, and aggressively
// merge and evict all posting lists from memory.
func periodicCommit() {
	ticker := time.NewTicker(time.Second)
	dirtyMap := make(map[fingerPrint]time.Time, 1000)
	// pending is used to ensure that we only have up to 15 goroutines doing gentle commits.
	pending := make(chan struct{}, 15)
	dsize := 0 // needed for better reporting.
	for {
		select {
		case key := <-dirtyChan:
			dirtyMap[key] = time.Now()

		case <-ticker.C:
			if len(dirtyMap) != dsize {
				dsize = len(dirtyMap)
				log.Printf("Dirty map size: %d\n", dsize)
			}

			var ms runtime.MemStats
			runtime.ReadMemStats(&ms)
			megs := (ms.HeapInuse + ms.StackInuse) / (1 << 20)

			inUse := float64(megs)
			idle := float64((ms.HeapIdle - ms.HeapReleased) / (1 << 20))

			fraction := math.Min(1.0, *commitFraction*math.Exp(float64(dsize)/1000000.0))
			gentleCommit(dirtyMap, pending, fraction)

			// Flush out the dirtyChan after acquiring lock. This allow posting lists which
			// are currently being processed to not get stuck on dirtyChan, which won't be
			// processed until aggressive evict finishes.

			// Okay, we exceed the max memory threshold.
			// Stop the world, and deal with this first.
			if inUse > 0.9*(*maxmemory) {
				log.Printf("Memory usage really close to threshold. STW. Allocated MB: %v\n", inUse)
				go evictShards(5)
			} else if inUse > 0.75*(*maxmemory) {
				log.Printf("Memory usage close to threshold. STW. Allocated MB: %v\n", inUse)
				go evictShards(1)
			} else {
				log.Printf("Cur: %v. Idle: %v, total: %v, STW: %v, NumGoroutines: %v\n",
					inUse, idle, inUse+idle, *maxmemory, runtime.NumGoroutine())
			}
		}
	}
}

type fingerPrint struct {
	fp  uint64
	gid uint32
}

const (
	syncChCapacity = 10000
)

var (
	pstore    *badger.KV
	syncCh    chan syncEntry
	dirtyChan chan fingerPrint // All dirty posting list keys are pushed here.
	marks     *syncMarks
	lhmaps    *listMaps
)

// Init initializes the posting lists package, the in memory and dirty list hash.
func Init(ps *badger.KV) {
	marks = new(syncMarks)
	pstore = ps
	lhmaps = new(listMaps)
	dirtyChan = make(chan fingerPrint, 10000)
	fmt.Println("Starting commit routine.")
	syncCh = make(chan syncEntry, syncChCapacity)

	go periodicCommit()
	go periodicFree()
	for i := 0; i < runtime.NumCPU(); i++ {
		go batchSync(i)
	}
}

// GetOrCreate stores the List corresponding to key, if it's not there already.
// to lhmap and returns it. It also returns a reference decrement function to be called by caller.
//
// plist, decr := GetOrCreate(key, store)
// defer decr()
// ... // Use plist
// TODO: This should take a node id and index. And just append all indices to a list.
// When doing a commit, it should update all the sync index watermarks.
// worker pkg would push the indices to the watermarks held by lists.
// And watermark stuff would have to be located outside worker pkg, maybe in x.
// That way, we don't have a dependency conflict.
func GetOrCreate(key []byte, group uint32) (rlist *List, decr func()) {
	fp := farm.Fingerprint64(key)
	lhmap := lhmapFor(group)
	lhmap.RLockShard(fp)
	defer lhmap.RUnlockShard(fp)

	lp := lhmap.Get(fp)
	if lp != nil {
		lp.incr()
		return lp, lp.decr
	}

	// Any initialization for l must be done before PutIfMissing. Once it's added
	// to the map, any other goroutine can retrieve it.
	l := getNew(key, pstore) // This retrieves a new *List and sets refcount to 1.
	l.water = marks.Get(group)

	lp = lhmap.PutIfMissing(fp, l)
	// We are always going to return lp to caller, whether it is l or not. So, let's
	// increment its reference counter.
	lp.incr()

	if lp != l {
		// Undo the increment in getNew() call above.
		go l.decr()
	} else {
		pk := x.Parse(key)
		if pk.IsIndex() || pk.IsCount() {
			err := pstore.Touch(key)
			x.Check(err)
		}
	}

	return lp, lp.decr
}

// Get takes a key and a groupID. It checks if the in-memory map has an
// updated value and returns it if it exists or it gets from the store and DOES NOT ADD to lhmap.
func Get(key []byte, gid uint32) (rlist *List, decr func()) {
	fp := farm.Fingerprint64(key)
	lhmap := lhmapFor(gid)

	lhmap.RLockShard(fp)
	lp := lhmap.Get(fp)
	lhmap.RUnlockShard(fp)

	if lp != nil {
		lp.incr()
		return lp, lp.decr
	}

	lp = getNew(key, pstore) // This retrieves a new *List and sets refcount to 1.
	return lp, lp.decr
}

func commitOne(l *List) {
	if l == nil {
		return
	}
	if _, err := l.SyncIfDirty(context.Background()); err != nil {
		log.Printf("Error while committing dirty list: %v\n", err)
	}
}

func CommitLists(numRoutines int, group uint32) {
	if group == 0 {
		return
	}

	// We iterate over lhmap, deleting keys and pushing values (List) into this
	// channel. Then goroutines right below will commit these lists to data store.
	workChan := make(chan *List, 10000)

	var wg sync.WaitGroup
	for i := 0; i < numRoutines; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for l := range workChan {
				commitOne(l)
				l.decr()
			}
		}()
	}

	lhmapFor(group).Each(func(k uint64, l *List) {
		if l == nil { // To be safe. Check might be unnecessary.
			return
		}
		l.incr()
		workChan <- l
	})
	close(workChan)
	wg.Wait()
}

func evictShard(group uint32, shardNum int) {
	lhmap := lhmapFor(group)
	lhmap.DeleteShard(shardNum, func(k uint64, l *List) {
		l.SetForDeletion()
		commitOne(l)
		l.decr()
	})
	log.Printf("evicted shard %d from group %d\n", shardNum, group)
}

func evictShards(numShards int) {
	var wg sync.WaitGroup
	for _, gid := range lhmaps.groups() {
		wg.Add(1)
		go func(group uint32) {
			defer wg.Done()
			for i := 0; i < numShards; i++ {
				shardNum := rand.Intn(lhmapNumShards)
				evictShard(group, shardNum)
			}
		}(gid)
	}
	wg.Wait()
}

// EvictAll removes all pl's stored in memory for given group
func EvictGroup(group uint32) {
	// This is serialized by raft so no need to worry about race condition from getOrCreate
	// request from same group
	lhmapFor(group).Each(func(k uint64, l *List) {
		l.SetForDeletion()
	})
	CommitLists(1, group)
	lhmapFor(group).EachWithDelete(func(k uint64, l *List) {
		l.decr()
	})
}

// The following logic is used to batch up all the writes to RocksDB.
type syncEntry struct {
	key     []byte
	val     []byte
	water   *x.WaterMark
	pending []uint64
}

func batchSync(i int) {
	var entries []syncEntry
	var loop uint64
	wb := make([]*badger.Entry, 0, 100)
	elog := trace.NewEventLog("Batch Sync", fmt.Sprintf("%d", i))

	for {
		ent := <-syncCh
	slurpLoop:
		for {
			entries = append(entries, ent)
			if len(entries) == syncChCapacity {
				// Avoid making infinite batch, push back against syncCh.
				break
			}
			select {
			case ent = <-syncCh:
			default:
				break slurpLoop
			}
		}

		loop++
		if loop%1000 == 0 {
			elog.Printf("[%4d] Writing batch of size: %v\n", loop, len(entries))
		}
		for _, e := range entries {
			if e.val == nil {
				wb = badger.EntriesDelete(wb, e.key)
			} else {
				wb = badger.EntriesSet(wb, e.key, e.val)
			}
		}
		pstore.BatchSet(wb)
		wb = wb[:0]

		for _, e := range entries {
			if e.water != nil {
				e.water.Ch <- x.Mark{Indices: e.pending, Done: true}
			}
		}
		entries = entries[:0]
	}
}
