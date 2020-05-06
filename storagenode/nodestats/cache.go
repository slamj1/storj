// Copyright (C) 2019 Storj Labs, Inc.
// See LICENSE for copying information.

package nodestats

import (
	"context"
	"math/rand"
	"time"

	"github.com/zeebo/errs"
	"go.uber.org/zap"
	"golang.org/x/sync/errgroup"

	"storj.io/common/storj"
	"storj.io/common/sync2"
	"storj.io/storj/private/date"
	"storj.io/storj/storagenode/heldamount"
	"storj.io/storj/storagenode/pricing"
	"storj.io/storj/storagenode/reputation"
	"storj.io/storj/storagenode/satellites"
	"storj.io/storj/storagenode/storageusage"
	"storj.io/storj/storagenode/trust"
)

// Config defines nodestats cache configuration
type Config struct {
	MaxSleep       time.Duration `help:"maximum duration to wait before requesting data" releaseDefault:"300s" devDefault:"1s"`
	ReputationSync time.Duration `help:"how often to sync reputation" releaseDefault:"4h" devDefault:"1m"`
	StorageSync    time.Duration `help:"how often to sync storage" releaseDefault:"12h" devDefault:"2m"`
}

// CacheStorage encapsulates cache DBs.
type CacheStorage struct {
	Reputation   reputation.DB
	StorageUsage storageusage.DB
	HeldAmount   heldamount.DB
	Pricing      pricing.DB
	Satellites   satellites.DB
}

// Cache runs cache loop and stores reputation stats
// and storage usage into db
//
// architecture: Chore
type Cache struct {
	log *zap.Logger

	db                CacheStorage
	service           *Service
	heldamountService *heldamount.Service
	trust             *trust.Pool

	maxSleep   time.Duration
	Reputation *sync2.Cycle
	Storage    *sync2.Cycle
}

// NewCache creates new caching service instance
func NewCache(log *zap.Logger, config Config, db CacheStorage, service *Service, heldamountService *heldamount.Service, trust *trust.Pool) *Cache {
	return &Cache{
		log:               log,
		db:                db,
		service:           service,
		heldamountService: heldamountService,
		trust:             trust,
		maxSleep:          config.MaxSleep,
		Reputation:        sync2.NewCycle(config.ReputationSync),
		Storage:           sync2.NewCycle(config.StorageSync),
	}
}

// Run runs loop
func (cache *Cache) Run(ctx context.Context) error {
	var group errgroup.Group

	err := cache.satelliteLoop(ctx, func(satelliteID storj.NodeID) error {
		stubHistory, err := cache.heldamountService.GetAllPaystubs(ctx, satelliteID)
		if err != nil {
			return err
		}

		for i := 0; i < len(stubHistory); i++ {
			err := cache.db.HeldAmount.StorePayStub(ctx, stubHistory[i])
			if err != nil {
				return err
			}
		}

		pricingModel, err := cache.service.GetPricingModel(ctx, satelliteID)
		if err != nil {
			return err
		}
		return cache.db.Pricing.Store(ctx, *pricingModel)
	})
	if err != nil {
		cache.log.Error("Get pricing-model/join date failed", zap.Error(err))
	}

	cache.Reputation.Start(ctx, &group, func(ctx context.Context) error {
		if err := cache.sleep(ctx); err != nil {
			return err
		}

		err := cache.CacheReputationStats(ctx)
		if err != nil {
			cache.log.Error("Get stats query failed", zap.Error(err))
		}

		return nil
	})
	cache.Storage.Start(ctx, &group, func(ctx context.Context) error {
		if err := cache.sleep(ctx); err != nil {
			return err
		}

		err := cache.CacheSpaceUsage(ctx)
		if err != nil {
			cache.log.Error("Get disk space usage query failed", zap.Error(err))
		}

		err = cache.CacheHeldAmount(ctx)
		if err != nil {
			cache.log.Error("Get held amount query failed", zap.Error(err))
		}

		return nil
	})

	return group.Wait()
}

// CacheReputationStats queries node stats from all the satellites
// known to the storagenode and stores information into db
func (cache *Cache) CacheReputationStats(ctx context.Context) (err error) {
	defer mon.Task()(&ctx)(&err)

	return cache.satelliteLoop(ctx, func(satellite storj.NodeID) error {
		stats, err := cache.service.GetReputationStats(ctx, satellite)
		if err != nil {
			return err
		}

		if err = cache.db.Reputation.Store(ctx, *stats); err != nil {
			cache.log.Error("err", zap.Error(err))
			return err
		}

		return nil
	})
}

// CacheSpaceUsage queries disk space usage from all the satellites
// known to the storagenode and stores information into db
func (cache *Cache) CacheSpaceUsage(ctx context.Context) (err error) {
	defer mon.Task()(&ctx)(&err)

	// get current month edges
	startDate, endDate := date.MonthBoundary(time.Now().UTC())

	return cache.satelliteLoop(ctx, func(satellite storj.NodeID) error {
		spaceUsages, err := cache.service.GetDailyStorageUsage(ctx, satellite, startDate, endDate)
		if err != nil {
			return err
		}

		err = cache.db.StorageUsage.Store(ctx, spaceUsages)
		if err != nil {
			return err
		}

		return nil
	})
}

// CacheHeldAmount queries held amount stats and payments from all the satellites known to the storagenode and stores info into db.
func (cache *Cache) CacheHeldAmount(ctx context.Context) (err error) {
	defer mon.Task()(&ctx)(&err)

	return cache.satelliteLoop(ctx, func(satellite storj.NodeID) error {
		now := time.Now().String()
		yearAndMonth, err := date.PeriodToTime(now)
		if err != nil {
			return err
		}

		previousMonth := yearAndMonth.AddDate(0, -1, 0).String()
		payStub, err := cache.heldamountService.GetPaystubStats(ctx, satellite, previousMonth)
		if err != nil {
			if heldamount.ErrNoPayStubForPeriod.Has(err) {
				return nil
			}

			return err
		}

		if payStub != nil {
			if err = cache.db.HeldAmount.StorePayStub(ctx, *payStub); err != nil {
				return err
			}
		}

		return nil
	})
}

// sleep for random interval in [0;maxSleep)
// returns error if context was cancelled
func (cache *Cache) sleep(ctx context.Context) error {
	if cache.maxSleep <= 0 {
		return nil
	}

	jitter := time.Duration(rand.Int63n(int64(cache.maxSleep)))
	if !sync2.Sleep(ctx, jitter) {
		return ctx.Err()
	}

	return nil
}

// satelliteLoop loops over all satellites from trust pool executing provided fn, caching errors if occurred,
// on each step checks if context has been cancelled
func (cache *Cache) satelliteLoop(ctx context.Context, fn func(id storj.NodeID) error) error {
	var groupErr errs.Group
	for _, satellite := range cache.trust.GetSatellites(ctx) {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}

		groupErr.Add(fn(satellite))
	}

	return groupErr.Err()
}

// Close closes underlying cycles
func (cache *Cache) Close() error {
	defer mon.Task()(nil)(nil)
	cache.Reputation.Close()
	cache.Storage.Close()
	return nil
}
