// Copyright (c) 2012, Suryandaru Triandana <syndtr@gmail.com>
// All rights reserved.
//
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package leveldb

import (
	"errors"
	"io"
	"os"
	"sync"
	"sync/atomic"

	"github.com/philsong/goleveldb/leveldb/iterator"
	"github.com/philsong/goleveldb/leveldb/journal"
	"github.com/philsong/goleveldb/leveldb/opt"
	"github.com/philsong/goleveldb/leveldb/storage"
	"github.com/philsong/goleveldb/leveldb/util"
)

// ErrManifest is the type that wraps errors produced by missing
// or corrupted manifest file.
type ErrManifest struct {
	Err error
}

func (e ErrManifest) Error() string {
	return e.Err.Error()
}

// session represent a persistent database session.
type session struct {
	// Need 64-bit alignment.
	stFileNum        uint64 // current unused file number
	stJournalNum     uint64 // current journal file number; need external synchronization
	stPrevJournalNum uint64 // prev journal file number; no longer used; for compatibility with older version of leveldb
	stSeq            uint64 // last mem compacted seq; need external synchronization

	stor     storage.Storage
	storLock util.Releaser
	o        *opt.Options
	cmp      *iComparer
	tops     *tOps

	manifest       *journal.Writer
	manifestWriter storage.Writer
	manifestFile   storage.File

	stCPtrs   [kNumLevels]iKey // compact pointers; need external synchronization
	stVersion *version         // current version
	vmu       sync.Mutex
}

func newSession(stor storage.Storage, o *opt.Options) (s *session, err error) {
	if stor == nil {
		return nil, os.ErrInvalid
	}
	storLock, err := stor.Lock()
	if err != nil {
		return
	}
	s = &session{
		stor:     stor,
		storLock: storLock,
	}
	s.setOptions(o)
	s.tops = newTableOps(s, s.o.GetMaxOpenFiles())
	s.setVersion(&version{s: s})
	return
}

// Close session.
func (s *session) close() {
	s.tops.close()
	if bc := s.o.GetBlockCache(); bc != nil {
		bc.Purge(nil)
	}
	if s.manifest != nil {
		s.manifest.Close()
	}
	if s.manifestWriter != nil {
		s.manifestWriter.Close()
	}
}

func (s *session) release() {
	s.storLock.Release()
}

// Create a new database session; need external synchronization.
func (s *session) create() error {
	// create manifest
	return s.newManifest(nil, nil)
}

// Recover a database session; need external synchronization.
func (s *session) recover() (err error) {
	defer func() {
		if os.IsNotExist(err) {
			// Don't return os.ErrNotExist if the underlying storage contains
			// other files that belong to LevelDB. So the DB won't get trashed.
			if files, _ := s.stor.GetFiles(storage.TypeAll); len(files) > 0 {
				err = ErrManifest{Err: errors.New("leveldb: manifest file missing")}
			}
		}
	}()

	file, err := s.stor.GetManifest()
	if err != nil {
		return
	}

	reader, err := file.Open()
	if err != nil {
		return
	}
	defer reader.Close()
	strict := s.o.GetStrict(opt.StrictManifest)
	jr := journal.NewReader(reader, dropper{s, file}, strict, true)

	staging := s.version_NB().newStaging()
	rec := &sessionRecord{}
	for {
		var r io.Reader
		r, err = jr.Next()
		if err != nil {
			if err == io.EOF {
				err = nil
				break
			}
			return
		}

		err = rec.decode(r)
		if err == nil {
			// save compact pointers
			for _, rp := range rec.compactionPointers {
				s.stCPtrs[rp.level] = iKey(rp.key)
			}
			// commit record to version staging
			staging.commit(rec)
		} else if strict {
			return ErrManifest{Err: err}
		} else {
			s.logf("manifest error: %v (skipped)", err)
		}
		rec.resetCompactionPointers()
		rec.resetAddedTables()
		rec.resetDeletedTables()
	}

	switch {
	case !rec.has(recComparer):
		return ErrManifest{Err: errors.New("leveldb: manifest missing comparer name")}
	case rec.comparer != s.cmp.cmp.Name():
		return ErrManifest{Err: errors.New("leveldb: comparer mismatch, " + "want '" + s.cmp.cmp.Name() + "', " + "got '" + rec.comparer + "'")}
	case !rec.has(recNextNum):
		return ErrManifest{Err: errors.New("leveldb: manifest missing next file number")}
	case !rec.has(recJournalNum):
		return ErrManifest{Err: errors.New("leveldb: manifest missing journal file number")}
	case !rec.has(recSeq):
		return ErrManifest{Err: errors.New("leveldb: manifest missing seq number")}
	}

	s.manifestFile = file
	s.setVersion(staging.finish())
	s.setFileNum(rec.nextNum)
	s.recordCommited(rec)
	return nil
}

// Commit session; need external synchronization.
func (s *session) commit(r *sessionRecord) (err error) {
	// spawn new version based on current version
	nv := s.version_NB().spawn(r)

	if s.manifest == nil {
		// manifest journal writer not yet created, create one
		err = s.newManifest(r, nv)
	} else {
		err = s.flushManifest(r)
	}

	// finally, apply new version if no error rise
	if err == nil {
		s.setVersion(nv)
	}

	return
}

// Pick a compaction based on current state; need external synchronization.
func (s *session) pickCompaction() *compaction {
	icmp := s.cmp
	ucmp := icmp.cmp

	v := s.version_NB()

	var level int
	var t0 tFiles
	if v.cScore >= 1 {
		level = v.cLevel
		cp := s.stCPtrs[level]
		tt := v.tables[level]
		for _, t := range tt {
			if cp == nil || icmp.Compare(t.max, cp) > 0 {
				t0 = append(t0, t)
				break
			}
		}
		if len(t0) == 0 {
			t0 = append(t0, tt[0])
		}
	} else {
		if p := atomic.LoadPointer(&v.cSeek); p != nil {
			ts := (*tSet)(p)
			level = ts.level
			t0 = append(t0, ts.table)
		} else {
			return nil
		}
	}

	c := &compaction{s: s, version: v, level: level}
	if level == 0 {
		min, max := t0.getRange(icmp)
		t0 = nil
		v.tables[0].getOverlaps(min.ukey(), max.ukey(), &t0, false, ucmp)
	}

	c.tables[0] = t0
	c.expand()
	return c
}

// Create compaction from given level and range; need external synchronization.
func (s *session) getCompactionRange(level int, min, max []byte) *compaction {
	v := s.version_NB()

	var t0 tFiles
	v.tables[level].getOverlaps(min, max, &t0, level != 0, s.cmp.cmp)
	if len(t0) == 0 {
		return nil
	}

	c := &compaction{s: s, version: v, level: level}
	c.tables[0] = t0
	c.expand()
	return c
}

// compaction represent a compaction state
type compaction struct {
	s       *session
	version *version

	level  int
	tables [2]tFiles

	gp              tFiles
	gpidx           int
	seenKey         bool
	overlappedBytes uint64
	min, max        iKey

	tPtrs [kNumLevels]int
}

// Expand compacted tables; need external synchronization.
func (c *compaction) expand() {
	s := c.s
	v := c.version
	icmp := s.cmp
	ucmp := icmp.cmp

	level := c.level
	vt0, vt1 := v.tables[level], v.tables[level+1]

	t0, t1 := c.tables[0], c.tables[1]
	min, max := t0.getRange(icmp)
	vt1.getOverlaps(min.ukey(), max.ukey(), &t1, true, ucmp)

	// Get entire range covered by compaction
	amin, amax := append(t0, t1...).getRange(icmp)

	// See if we can grow the number of inputs in "level" without
	// changing the number of "level+1" files we pick up.
	if len(t1) > 0 {
		var exp0 tFiles
		vt0.getOverlaps(amin.ukey(), amax.ukey(), &exp0, level != 0, ucmp)
		if len(exp0) > len(t0) && t1.size()+exp0.size() < kExpCompactionMaxBytes {
			var exp1 tFiles
			xmin, xmax := exp0.getRange(icmp)
			vt1.getOverlaps(xmin.ukey(), xmax.ukey(), &exp1, true, ucmp)
			if len(exp1) == len(t1) {
				s.logf("table@compaction expanding L%d+L%d (F·%d S·%s)+(F·%d S·%s) -> (F·%d S·%s)+(F·%d S·%s)",
					level, level+1, len(t0), shortenb(int(t0.size())), len(t1), shortenb(int(t1.size())),
					len(exp0), shortenb(int(exp0.size())), len(exp1), shortenb(int(exp1.size())))
				min, max = xmin, xmax
				t0, t1 = exp0, exp1
				amin, amax = append(t0, t1...).getRange(icmp)
			}
		}
	}

	// Compute the set of grandparent files that overlap this compaction
	// (parent == level+1; grandparent == level+2)
	if level+2 < kNumLevels {
		v.tables[level+2].getOverlaps(amin.ukey(), amax.ukey(), &c.gp, true, ucmp)
	}

	c.tables[0], c.tables[1] = t0, t1
	c.min, c.max = min, max
}

// Check whether compaction is trivial.
func (c *compaction) trivial() bool {
	return len(c.tables[0]) == 1 && len(c.tables[1]) == 0 && c.gp.size() <= kMaxGrandParentOverlapBytes
}

func (c *compaction) isBaseLevelForKey(key []byte) bool {
	s := c.s
	v := c.version
	ucmp := s.cmp.cmp
	for level, tt := range v.tables[c.level+2:] {
		for c.tPtrs[level] < len(tt) {
			t := tt[c.tPtrs[level]]
			if ucmp.Compare(key, t.max.ukey()) <= 0 {
				// We've advanced far enough
				if ucmp.Compare(key, t.min.ukey()) >= 0 {
					// Key falls in this file's range, so definitely not base level
					return false
				}
				break
			}
			c.tPtrs[level]++
		}
	}
	return true
}

func (c *compaction) shouldStopBefore(key iKey) bool {
	icmp := c.s.cmp
	for ; c.gpidx < len(c.gp); c.gpidx++ {
		gp := c.gp[c.gpidx]
		if icmp.Compare(key, gp.max) <= 0 {
			break
		}
		if c.seenKey {
			c.overlappedBytes += gp.size
		}
	}
	c.seenKey = true

	if c.overlappedBytes > kMaxGrandParentOverlapBytes {
		// Too much overlap for current output; start new output
		c.overlappedBytes = 0
		return true
	}
	return false
}

func (c *compaction) newIterator() iterator.Iterator {
	s := c.s
	icmp := s.cmp

	level := c.level
	icap := 2
	if c.level == 0 {
		icap = len(c.tables[0]) + 1
	}
	its := make([]iterator.Iterator, 0, icap)

	ro := &opt.ReadOptions{
		DontFillCache: true,
	}
	strict := s.o.GetStrict(opt.StrictIterator)

	for i, tt := range c.tables {
		if len(tt) == 0 {
			continue
		}

		if level+i == 0 {
			for _, t := range tt {
				its = append(its, s.tops.newIterator(t, nil, ro))
			}
		} else {
			it := iterator.NewIndexedIterator(tt.newIndexIterator(s.tops, icmp, nil, ro), strict, true)
			its = append(its, it)
		}
	}

	return iterator.NewMergedIterator(its, icmp, true)
}
