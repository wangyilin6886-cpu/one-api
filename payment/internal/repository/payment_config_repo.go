package repository

import (
	"context"
	"errors"
	"sync"
	"time"

	"gorm.io/gorm"

	"github.com/songquanpeng/one-api/payment/internal/model"
)

// PaymentConfigRepo gives synchronized read access to the payment_config
// table with an in-memory cache (TTL configurable). Writes invalidate the
// cache for the touched key.
type PaymentConfigRepo struct {
	db       *gorm.DB
	cacheTTL time.Duration

	mu       sync.RWMutex
	snapshot map[string]string
	loadedAt time.Time
}

func NewPaymentConfigRepo(db *gorm.DB, cacheTTL time.Duration) *PaymentConfigRepo {
	return &PaymentConfigRepo{
		db:       db,
		cacheTTL: cacheTTL,
		snapshot: map[string]string{},
	}
}

// SeedIfMissing inserts the default rows from model.SeedRows that don't yet
// exist. Existing rows are left untouched (operators may have customized them).
func (r *PaymentConfigRepo) SeedIfMissing(ctx context.Context) error {
	now := time.Now().UTC()
	for _, row := range model.SeedRows(now) {
		var count int64
		err := r.db.WithContext(ctx).
			Model(&model.PaymentConfig{}).
			Where("config_key = ?", row.ConfigKey).
			Count(&count).Error
		if err != nil {
			return err
		}
		if count > 0 {
			continue
		}
		if err := r.db.WithContext(ctx).Create(&row).Error; err != nil {
			// If a parallel seeder beat us by milliseconds, ignore duplicate.
			if !IsDuplicateKeyError(err) {
				return err
			}
		}
	}
	r.invalidate()
	return nil
}

// Get returns the value for a key, refreshing the cache if needed. Returns
// "" + nil if the key doesn't exist - callers must handle empty as
// "operator hasn't filled this in yet".
func (r *PaymentConfigRepo) Get(ctx context.Context, key string) (string, error) {
	if err := r.refreshIfStale(ctx); err != nil {
		return "", err
	}
	r.mu.RLock()
	v := r.snapshot[key]
	r.mu.RUnlock()
	return v, nil
}

// GetAll returns a copy of the snapshot, refreshing the cache if stale.
func (r *PaymentConfigRepo) GetAll(ctx context.Context) (map[string]string, error) {
	if err := r.refreshIfStale(ctx); err != nil {
		return nil, err
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make(map[string]string, len(r.snapshot))
	for k, v := range r.snapshot {
		out[k] = v
	}
	return out, nil
}

// Set writes (or upserts) a single key. updatedBy is the admin user id (0
// for system writes).
func (r *PaymentConfigRepo) Set(ctx context.Context, key, value string, updatedBy int) error {
	if key == "" {
		return errors.New("config key is empty")
	}
	row := model.PaymentConfig{
		ConfigKey:   key,
		ConfigValue: value,
		UpdatedAt:   time.Now().UTC(),
		UpdatedBy:   updatedBy,
	}
	// MySQL "ON DUPLICATE KEY UPDATE" equivalent via GORM Save: since the
	// primary key is ConfigKey, GORM does UPSERT semantics.
	if err := r.db.WithContext(ctx).Save(&row).Error; err != nil {
		return err
	}
	r.invalidate()
	return nil
}

// InvalidateCache forces the next Get / GetAll to re-fetch from DB.
func (r *PaymentConfigRepo) InvalidateCache() { r.invalidate() }

func (r *PaymentConfigRepo) refreshIfStale(ctx context.Context) error {
	r.mu.RLock()
	fresh := time.Since(r.loadedAt) < r.cacheTTL && len(r.snapshot) > 0
	r.mu.RUnlock()
	if fresh {
		return nil
	}
	return r.reloadFromDB(ctx)
}

func (r *PaymentConfigRepo) reloadFromDB(ctx context.Context) error {
	var rows []model.PaymentConfig
	if err := r.db.WithContext(ctx).Find(&rows).Error; err != nil {
		return err
	}
	snap := make(map[string]string, len(rows))
	for _, row := range rows {
		snap[row.ConfigKey] = row.ConfigValue
	}
	r.mu.Lock()
	r.snapshot = snap
	r.loadedAt = time.Now()
	r.mu.Unlock()
	return nil
}

func (r *PaymentConfigRepo) invalidate() {
	r.mu.Lock()
	r.loadedAt = time.Time{}
	r.mu.Unlock()
}
