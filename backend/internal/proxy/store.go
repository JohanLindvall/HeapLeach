// SPDX-License-Identifier: MIT

package proxy

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/JohanLindvall/HeapLeach/internal/config"
	bolt "go.etcd.io/bbolt"
)

var entriesBucket = []byte("proxies-v1")
var sourcesBucket = []byte("sources-v1")
var priorsBucket = []byte("priors-v1")

func (p *Pool) openStore(path string) error {
	if path == "" {
		return errors.New("proxy database path is empty")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	db, err := bolt.Open(path, 0o600, &bolt.Options{Timeout: config.ProxyDBTimeout})
	if err != nil {
		return fmt.Errorf("open proxy database: %w", err)
	}
	p.db = db
	err = db.Update(func(tx *bolt.Tx) error {
		for _, name := range [][]byte{entriesBucket, sourcesBucket, priorsBucket} {
			if _, err := tx.CreateBucketIfNotExists(name); err != nil {
				return err
			}
		}
		return nil
	})
	if err == nil {
		err = db.View(func(tx *bolt.Tx) error {
			if err := tx.Bucket(entriesBucket).ForEach(func(k, v []byte) error {
				var e entry
				if err := json.Unmarshal(v, &e); err != nil {
					return err
				}
				u, err := normalize(e.URL)
				if err != nil || u != string(k) {
					return errors.New("invalid proxy inventory record")
				}
				p.entries[e.URL] = &e
				return nil
			}); err != nil {
				return err
			}
			if err := tx.Bucket(sourcesBucket).ForEach(func(k, v []byte) error {
				var s source
				if err := json.Unmarshal(v, &s); err != nil {
					return err
				}
				p.sources[string(k)] = &s
				return nil
			}); err != nil {
				return err
			}
			return tx.Bucket(priorsBucket).ForEach(func(k, v []byte) error {
				var s prior
				if err := json.Unmarshal(v, &s); err != nil {
					return err
				}
				p.priors[string(k)] = &s
				return nil
			})
		})
	}
	if err != nil {
		db.Close()
		return fmt.Errorf("restore proxy database: %w", err)
	}
	return nil
}

func putJSON(b *bolt.Bucket, key string, v any) error {
	data, err := json.Marshal(v)
	if err != nil {
		return err
	}
	return b.Put([]byte(key), data)
}

// persist commits each observed outcome, including cooldowns and first-try
// priors, so an unclean shutdown does not erase the evidence used to select.
func (p *Pool) persist(e *entry, site string) {
	err := p.db.Update(func(tx *bolt.Tx) error {
		if err := putJSON(tx.Bucket(entriesBucket), e.URL, e); err != nil {
			return err
		}
		if first := p.priors[site]; first != nil {
			return putJSON(tx.Bucket(priorsBucket), site, first)
		}
		return nil
	})
	if err != nil {
		p.log.Error("could not save proxy health", "err", err)
	} else {
		delete(p.dirty, e.URL)
	}
}

// Live speed samples must not write Bolt transactions on the UI/progress
// thread. Flush only changed entries from the background task or on close.
func (p *Pool) flushMeasurements() error {
	if len(p.dirty) == 0 {
		return nil
	}
	err := p.db.Update(func(tx *bolt.Tx) error {
		for raw := range p.dirty {
			if e := p.entries[raw]; e != nil {
				if err := putJSON(tx.Bucket(entriesBucket), raw, e); err != nil {
					return err
				}
			}
		}
		return nil
	})
	if err == nil {
		clear(p.dirty)
	}
	return err
}

func (p *Pool) saveAll() error {
	return p.db.Update(func(tx *bolt.Tx) error {
		for _, name := range [][]byte{entriesBucket, sourcesBucket} {
			if err := tx.DeleteBucket(name); err != nil {
				return err
			}
			if _, err := tx.CreateBucket(name); err != nil {
				return err
			}
		}
		for key, e := range p.entries {
			if err := putJSON(tx.Bucket(entriesBucket), key, e); err != nil {
				return err
			}
		}
		for key, s := range p.sources {
			if err := putJSON(tx.Bucket(sourcesBucket), key, s); err != nil {
				return err
			}
		}
		return nil
	})
}
