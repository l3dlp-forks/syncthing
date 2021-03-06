// Copyright (C) 2014 The Syncthing Authors.
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this file,
// You can obtain one at http://mozilla.org/MPL/2.0/.

// Package db provides a set type to track local/remote files with newness
// checks. We must do a certain amount of normalization in here. We will get
// fed paths with either native or wire-format separators and encodings
// depending on who calls us. We transform paths to wire-format (NFC and
// slashes) on the way to the database, and transform to native format
// (varying separator and encoding) on the way back out.
package db

import (
	stdsync "sync"
	"sync/atomic"

	"github.com/syncthing/syncthing/lib/osutil"
	"github.com/syncthing/syncthing/lib/protocol"
	"github.com/syncthing/syncthing/lib/sync"
)

type FileSet struct {
	localVersion int64 // Our local version counter
	folder       string
	db           *Instance
	blockmap     *BlockMap
	localSize    sizeTracker
	globalSize   sizeTracker

	remoteLocalVersion map[protocol.DeviceID]int64 // Highest seen local versions for other devices
	updateMutex        sync.Mutex                  // protects remoteLocalVersion and database updates
}

// FileIntf is the set of methods implemented by both protocol.FileInfo and
// FileInfoTruncated.
type FileIntf interface {
	FileSize() int64
	FileName() string
	IsDeleted() bool
	IsInvalid() bool
	IsDirectory() bool
	IsSymlink() bool
	HasPermissionBits() bool
}

// The Iterator is called with either a protocol.FileInfo or a
// FileInfoTruncated (depending on the method) and returns true to
// continue iteration, false to stop.
type Iterator func(f FileIntf) bool

type sizeTracker struct {
	files   int
	deleted int
	bytes   int64
	mut     stdsync.Mutex
}

func (s *sizeTracker) addFile(f FileIntf) {
	if f.IsInvalid() {
		return
	}

	s.mut.Lock()
	if f.IsDeleted() {
		s.deleted++
	} else {
		s.files++
	}
	s.bytes += f.FileSize()
	s.mut.Unlock()
}

func (s *sizeTracker) removeFile(f FileIntf) {
	if f.IsInvalid() {
		return
	}

	s.mut.Lock()
	if f.IsDeleted() {
		s.deleted--
	} else {
		s.files--
	}
	s.bytes -= f.FileSize()
	if s.deleted < 0 || s.files < 0 {
		panic("bug: removed more than added")
	}
	s.mut.Unlock()
}

func (s *sizeTracker) Size() (files, deleted int, bytes int64) {
	s.mut.Lock()
	defer s.mut.Unlock()
	return s.files, s.deleted, s.bytes
}

func NewFileSet(folder string, db *Instance) *FileSet {
	var s = FileSet{
		remoteLocalVersion: make(map[protocol.DeviceID]int64),
		folder:             folder,
		db:                 db,
		blockmap:           NewBlockMap(db, db.folderIdx.ID([]byte(folder))),
		updateMutex:        sync.NewMutex(),
	}

	s.db.checkGlobals([]byte(folder), &s.globalSize)

	var deviceID protocol.DeviceID
	s.db.withAllFolderTruncated([]byte(folder), func(device []byte, f FileInfoTruncated) bool {
		copy(deviceID[:], device)
		if deviceID == protocol.LocalDeviceID {
			if f.LocalVersion > s.localVersion {
				s.localVersion = f.LocalVersion
			}
			s.localSize.addFile(f)
		} else if f.LocalVersion > s.remoteLocalVersion[deviceID] {
			s.remoteLocalVersion[deviceID] = f.LocalVersion
		}
		return true
	})
	l.Debugf("loaded localVersion for %q: %#v", folder, s.localVersion)

	return &s
}

func (s *FileSet) Replace(device protocol.DeviceID, fs []protocol.FileInfo) {
	l.Debugf("%s Replace(%v, [%d])", s.folder, device, len(fs))
	normalizeFilenames(fs)

	s.updateMutex.Lock()
	defer s.updateMutex.Unlock()

	if device == protocol.LocalDeviceID {
		if len(fs) == 0 {
			s.localVersion = 0
		} else {
			// Always overwrite LocalVersion on updated files to ensure
			// correct ordering. The caller is supposed to leave it set to
			// zero anyhow.
			for i := range fs {
				fs[i].LocalVersion = atomic.AddInt64(&s.localVersion, 1)
			}
		}
	} else {
		s.remoteLocalVersion[device] = maxLocalVersion(fs)
	}
	s.db.replace([]byte(s.folder), device[:], fs, &s.localSize, &s.globalSize)
	if device == protocol.LocalDeviceID {
		s.blockmap.Drop()
		s.blockmap.Add(fs)
	}
}

func (s *FileSet) Update(device protocol.DeviceID, fs []protocol.FileInfo) {
	l.Debugf("%s Update(%v, [%d])", s.folder, device, len(fs))
	normalizeFilenames(fs)

	s.updateMutex.Lock()
	defer s.updateMutex.Unlock()

	if device == protocol.LocalDeviceID {
		discards := make([]protocol.FileInfo, 0, len(fs))
		updates := make([]protocol.FileInfo, 0, len(fs))
		for i, newFile := range fs {
			fs[i].LocalVersion = atomic.AddInt64(&s.localVersion, 1)
			existingFile, ok := s.db.getFile([]byte(s.folder), device[:], []byte(newFile.Name))
			if !ok || !existingFile.Version.Equal(newFile.Version) {
				discards = append(discards, existingFile)
				updates = append(updates, newFile)
			}
		}
		s.blockmap.Discard(discards)
		s.blockmap.Update(updates)
	} else {
		s.remoteLocalVersion[device] = maxLocalVersion(fs)
	}
	s.db.updateFiles([]byte(s.folder), device[:], fs, &s.localSize, &s.globalSize)
}

func (s *FileSet) WithNeed(device protocol.DeviceID, fn Iterator) {
	l.Debugf("%s WithNeed(%v)", s.folder, device)
	s.db.withNeed([]byte(s.folder), device[:], false, nativeFileIterator(fn))
}

func (s *FileSet) WithNeedTruncated(device protocol.DeviceID, fn Iterator) {
	l.Debugf("%s WithNeedTruncated(%v)", s.folder, device)
	s.db.withNeed([]byte(s.folder), device[:], true, nativeFileIterator(fn))
}

func (s *FileSet) WithHave(device protocol.DeviceID, fn Iterator) {
	l.Debugf("%s WithHave(%v)", s.folder, device)
	s.db.withHave([]byte(s.folder), device[:], nil, false, nativeFileIterator(fn))
}

func (s *FileSet) WithHaveTruncated(device protocol.DeviceID, fn Iterator) {
	l.Debugf("%s WithHaveTruncated(%v)", s.folder, device)
	s.db.withHave([]byte(s.folder), device[:], nil, true, nativeFileIterator(fn))
}

func (s *FileSet) WithPrefixedHaveTruncated(device protocol.DeviceID, prefix string, fn Iterator) {
	l.Debugf("%s WithPrefixedHaveTruncated(%v)", s.folder, device)
	s.db.withHave([]byte(s.folder), device[:], []byte(osutil.NormalizedFilename(prefix)), true, nativeFileIterator(fn))
}
func (s *FileSet) WithGlobal(fn Iterator) {
	l.Debugf("%s WithGlobal()", s.folder)
	s.db.withGlobal([]byte(s.folder), nil, false, nativeFileIterator(fn))
}

func (s *FileSet) WithGlobalTruncated(fn Iterator) {
	l.Debugf("%s WithGlobalTruncated()", s.folder)
	s.db.withGlobal([]byte(s.folder), nil, true, nativeFileIterator(fn))
}

func (s *FileSet) WithPrefixedGlobalTruncated(prefix string, fn Iterator) {
	l.Debugf("%s WithPrefixedGlobalTruncated()", s.folder, prefix)
	s.db.withGlobal([]byte(s.folder), []byte(osutil.NormalizedFilename(prefix)), true, nativeFileIterator(fn))
}

func (s *FileSet) Get(device protocol.DeviceID, file string) (protocol.FileInfo, bool) {
	f, ok := s.db.getFile([]byte(s.folder), device[:], []byte(osutil.NormalizedFilename(file)))
	f.Name = osutil.NativeFilename(f.Name)
	return f, ok
}

func (s *FileSet) GetGlobal(file string) (protocol.FileInfo, bool) {
	fi, ok := s.db.getGlobal([]byte(s.folder), []byte(osutil.NormalizedFilename(file)), false)
	if !ok {
		return protocol.FileInfo{}, false
	}
	f := fi.(protocol.FileInfo)
	f.Name = osutil.NativeFilename(f.Name)
	return f, true
}

func (s *FileSet) GetGlobalTruncated(file string) (FileInfoTruncated, bool) {
	fi, ok := s.db.getGlobal([]byte(s.folder), []byte(osutil.NormalizedFilename(file)), true)
	if !ok {
		return FileInfoTruncated{}, false
	}
	f := fi.(FileInfoTruncated)
	f.Name = osutil.NativeFilename(f.Name)
	return f, true
}

func (s *FileSet) Availability(file string) []protocol.DeviceID {
	return s.db.availability([]byte(s.folder), []byte(osutil.NormalizedFilename(file)))
}

func (s *FileSet) LocalVersion(device protocol.DeviceID) int64 {
	if device == protocol.LocalDeviceID {
		return atomic.LoadInt64(&s.localVersion)
	}

	s.updateMutex.Lock()
	defer s.updateMutex.Unlock()
	return s.remoteLocalVersion[device]
}

func (s *FileSet) LocalSize() (files, deleted int, bytes int64) {
	return s.localSize.Size()
}

func (s *FileSet) GlobalSize() (files, deleted int, bytes int64) {
	return s.globalSize.Size()
}

func (s *FileSet) IndexID(device protocol.DeviceID) protocol.IndexID {
	id := s.db.getIndexID(device[:], []byte(s.folder))
	if id == 0 && device == protocol.LocalDeviceID {
		// No index ID set yet. We create one now.
		id = protocol.NewIndexID()
		s.db.setIndexID(device[:], []byte(s.folder), id)
	}
	return id
}

func (s *FileSet) SetIndexID(device protocol.DeviceID, id protocol.IndexID) {
	if device == protocol.LocalDeviceID {
		panic("do not explicitly set index ID for local device")
	}
	s.db.setIndexID(device[:], []byte(s.folder), id)
}

// maxLocalVersion returns the highest of the LocalVersion numbers found in
// the given slice of FileInfos. This should really be the LocalVersion of
// the last item, but Syncthing v0.14.0 and other implementations may not
// implement update sorting....
func maxLocalVersion(fs []protocol.FileInfo) int64 {
	var max int64
	for _, f := range fs {
		if f.LocalVersion > max {
			max = f.LocalVersion
		}
	}
	return max
}

// DropFolder clears out all information related to the given folder from the
// database.
func DropFolder(db *Instance, folder string) {
	db.dropFolder([]byte(folder))
	bm := &BlockMap{
		db:     db,
		folder: db.folderIdx.ID([]byte(folder)),
	}
	bm.Drop()
	NewVirtualMtimeRepo(db, folder).Drop()
}

func normalizeFilenames(fs []protocol.FileInfo) {
	for i := range fs {
		fs[i].Name = osutil.NormalizedFilename(fs[i].Name)
	}
}

func nativeFileIterator(fn Iterator) Iterator {
	return func(fi FileIntf) bool {
		switch f := fi.(type) {
		case protocol.FileInfo:
			f.Name = osutil.NativeFilename(f.Name)
			return fn(f)
		case FileInfoTruncated:
			f.Name = osutil.NativeFilename(f.Name)
			return fn(f)
		default:
			panic("unknown interface type")
		}
	}
}
