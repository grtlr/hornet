package indexer

import (
	"time"

	iotago "github.com/iotaledger/iota.go/v3"
)

type foundry struct {
	FoundryID foundryIDBytes `gorm:"primaryKey;notnull"`
	OutputID  outputIDBytes  `gorm:"unique;notnull"`
	Amount    uint64         `gorm:"notnull"`
	Address   addressBytes   `gorm:"notnull;index:foundries_address"`
	CreatedAt time.Time      `gorm:"notnull"`
}

type FoundryFilterOptions struct {
	unlockableByAddress *iotago.Address
	pageSize            int
	cursor              *string
	createdBefore       *time.Time
	createdAfter        *time.Time
}

type FoundryFilterOption func(*FoundryFilterOptions)

func FoundryUnlockableByAddress(address iotago.Address) FoundryFilterOption {
	return func(args *FoundryFilterOptions) {
		args.unlockableByAddress = &address
	}
}

func FoundryPageSize(pageSize int) FoundryFilterOption {
	return func(args *FoundryFilterOptions) {
		args.pageSize = pageSize
	}
}

func FoundryCursor(cursor string) FoundryFilterOption {
	return func(args *FoundryFilterOptions) {
		args.cursor = &cursor
	}
}

func FoundryCreatedBefore(time time.Time) FoundryFilterOption {
	return func(args *FoundryFilterOptions) {
		args.createdBefore = &time
	}
}

func FoundryCreatedAfter(time time.Time) FoundryFilterOption {
	return func(args *FoundryFilterOptions) {
		args.createdAfter = &time
	}
}

func foundryFilterOptions(optionalOptions []FoundryFilterOption) *FoundryFilterOptions {
	result := &FoundryFilterOptions{}

	for _, optionalOption := range optionalOptions {
		optionalOption(result)
	}
	return result
}

func (i *Indexer) FoundryOutput(foundryID *iotago.FoundryID) *IndexerResult {
	query := i.db.Model(&foundry{}).
		Where("foundry_id = ?", foundryID[:]).
		Limit(1)

	return i.combineOutputIDFilteredQuery(query, 0, nil)
}

func (i *Indexer) FoundryOutputsWithFilters(filters ...FoundryFilterOption) *IndexerResult {
	opts := foundryFilterOptions(filters)
	query := i.db.Model(&foundry{})

	if opts.unlockableByAddress != nil {
		addr, err := addressBytesForAddress(*opts.unlockableByAddress)
		if err != nil {
			return errorResult(err)
		}
		query = query.Where("address = ?", addr[:])
	}

	if opts.createdBefore != nil {
		query = query.Where("created_at < ?", *opts.createdBefore)
	}

	if opts.createdAfter != nil {
		query = query.Where("created_at > ?", *opts.createdAfter)
	}

	return i.combineOutputIDFilteredQuery(query, opts.pageSize, opts.cursor)
}
