package storage

import (
	"encoding/binary"
	"time"

	"github.com/gohornet/hornet/pkg/common"
	"github.com/gohornet/hornet/pkg/model/milestone"
	"github.com/gohornet/hornet/pkg/profile"
	"github.com/iotaledger/hive.go/kvstore"
	"github.com/iotaledger/hive.go/objectstorage"
	iotago "github.com/iotaledger/iota.go/v3"
)

// CachedUnreferencedBlock represents a cached unreferenced block.
type CachedUnreferencedBlock struct {
	objectstorage.CachedObject
}

type CachedUnreferencedBlocks []*CachedUnreferencedBlock

// Release releases the cached unreferenced blocks, to be picked up by the persistence layer (as soon as all consumers are done).
func (cachedUnreferencedBlocks CachedUnreferencedBlocks) Release(force ...bool) {
	for _, cachedUnreferencedBlock := range cachedUnreferencedBlocks {
		cachedUnreferencedBlock.Release(force...) // unreferencedBlock -1
	}
}

// UnreferencedBlock retrieves the unreferenced block, that is cached in this container.
func (c *CachedUnreferencedBlock) UnreferencedBlock() *UnreferencedBlock {
	return c.Get().(*UnreferencedBlock)
}

func unreferencedBlockFactory(key []byte, _ []byte) (objectstorage.StorableObject, error) {
	blockID := iotago.BlockID{}
	copy(blockID[:], key[4:36])
	unreferencedBlock := NewUnreferencedBlock(milestone.Index(binary.LittleEndian.Uint32(key[:4])), blockID)
	return unreferencedBlock, nil
}

func (s *Storage) UnreferencedBlocksStorageSize() int {
	return s.unreferencedBlocksStorage.GetSize()
}

func (s *Storage) configureUnreferencedBlocksStorage(store kvstore.KVStore, opts *profile.CacheOpts) error {

	cacheTime, err := time.ParseDuration(opts.CacheTime)
	if err != nil {
		return err
	}

	leakDetectionMaxConsumerHoldTime, err := time.ParseDuration(opts.LeakDetectionOptions.MaxConsumerHoldTime)
	if err != nil {
		return err
	}

	unreferencedBlocksStore, err := store.WithRealm([]byte{common.StorePrefixUnreferencedBlocks})
	if err != nil {
		return err
	}

	s.unreferencedBlocksStorage = objectstorage.New(
		unreferencedBlocksStore,
		unreferencedBlockFactory,
		objectstorage.CacheTime(cacheTime),
		objectstorage.PartitionKey(4, 32),
		objectstorage.PersistenceEnabled(true),
		objectstorage.KeysOnly(true),
		objectstorage.StoreOnCreation(true),
		objectstorage.ReleaseExecutorWorkerCount(opts.ReleaseExecutorWorkerCount),
		objectstorage.LeakDetectionEnabled(opts.LeakDetectionOptions.Enabled,
			objectstorage.LeakDetectionOptions{
				MaxConsumersPerObject: opts.LeakDetectionOptions.MaxConsumersPerObject,
				MaxConsumerHoldTime:   leakDetectionMaxConsumerHoldTime,
			}),
	)

	return nil
}

// UnreferencedBlockIDs returns all block IDs of unreferenced blocks for that milestone.
func (s *Storage) UnreferencedBlockIDs(msIndex milestone.Index, iteratorOptions ...IteratorOption) iotago.BlockIDs {

	var unreferencedBlockIDs iotago.BlockIDs

	key := make([]byte, 4)
	binary.LittleEndian.PutUint32(key, uint32(msIndex))

	s.unreferencedBlocksStorage.ForEachKeyOnly(func(key []byte) bool {
		blockID := iotago.BlockID{}
		copy(blockID[:], key[4:36])
		unreferencedBlockIDs = append(unreferencedBlockIDs, blockID)
		return true
	}, append(ObjectStorageIteratorOptions(iteratorOptions...), objectstorage.WithIteratorPrefix(key))...)

	return unreferencedBlockIDs
}

// UnreferencedBlockConsumer consumes the given unreferenced block during looping through all unreferenced blocks.
type UnreferencedBlockConsumer func(msIndex milestone.Index, blockID iotago.BlockID) bool

// ForEachUnreferencedBlock loops over all unreferenced blocks.
func (s *Storage) ForEachUnreferencedBlock(consumer UnreferencedBlockConsumer, iteratorOptions ...IteratorOption) {
	s.unreferencedBlocksStorage.ForEachKeyOnly(func(key []byte) bool {
		blockID := iotago.BlockID{}
		copy(blockID[:], key[4:36])
		return consumer(milestone.Index(binary.LittleEndian.Uint32(key[:4])), blockID)
	}, ObjectStorageIteratorOptions(iteratorOptions...)...)
}

// ForEachUnreferencedBlock loops over all unreferenced blocks.
func (ns *NonCachedStorage) ForEachUnreferencedBlock(consumer UnreferencedBlockConsumer, iteratorOptions ...IteratorOption) {
	ns.storage.unreferencedBlocksStorage.ForEachKeyOnly(func(key []byte) bool {
		blockID := iotago.BlockID{}
		copy(blockID[:], key[4:36])
		return consumer(milestone.Index(binary.LittleEndian.Uint32(key[:4])), blockID)
	}, append(ObjectStorageIteratorOptions(iteratorOptions...), objectstorage.WithIteratorSkipCache(true))...)
}

// StoreUnreferencedBlock stores the unreferenced block in the persistence layer and returns a cached object.
// unreferencedBlock +1
func (s *Storage) StoreUnreferencedBlock(msIndex milestone.Index, blockID iotago.BlockID) *CachedUnreferencedBlock {
	unreferencedBlock := NewUnreferencedBlock(msIndex, blockID)
	return &CachedUnreferencedBlock{CachedObject: s.unreferencedBlocksStorage.Store(unreferencedBlock)}
}

// DeleteUnreferencedBlocks deletes unreferenced block entries in the cache/persistence layer.
func (s *Storage) DeleteUnreferencedBlocks(msIndex milestone.Index, iteratorOptions ...IteratorOption) int {

	msIndexBytes := make([]byte, 4)
	binary.LittleEndian.PutUint32(msIndexBytes, uint32(msIndex))

	var keysToDelete [][]byte

	s.unreferencedBlocksStorage.ForEachKeyOnly(func(key []byte) bool {
		keysToDelete = append(keysToDelete, key)
		return true
	}, append(ObjectStorageIteratorOptions(iteratorOptions...), objectstorage.WithIteratorPrefix(msIndexBytes))...)

	for _, key := range keysToDelete {
		s.unreferencedBlocksStorage.Delete(key)
	}

	return len(keysToDelete)
}

// ShutdownUnreferencedBlocksStorage shuts down the unreferenced blocks storage.
func (s *Storage) ShutdownUnreferencedBlocksStorage() {
	s.unreferencedBlocksStorage.Shutdown()
}

// FlushUnreferencedBlocksStorage flushes the unreferenced blocks storage.
func (s *Storage) FlushUnreferencedBlocksStorage() {
	s.unreferencedBlocksStorage.Flush()
}