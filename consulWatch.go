package consulWatch

import (
	"errors"
	"strings"
	"time"

	"github.com/cenkalti/backoff"
	"github.com/hashicorp/consul/api"
)

type ConsulWatched struct {
	Consul      *api.Client
	Prefix      string
	UpdateChan  chan map[string]interface{}
	CreateChan  chan map[string]interface{}
	DeleteChan  chan map[string]interface{}
	ErrChan     chan error
	WaitTimeout time.Duration

	updateIndex map[string]uint64
	stopChan    chan bool
}

var WaitTimeout = 10 * time.Minute
var watchers = make([]*ConsulWatched, 1)

func (w *ConsulWatched) Watch() error {
	// Check prefix format
	if w.Prefix == "" {
		return errors.New("invalid Prefix")
	}

	if w.Prefix[len(w.Prefix)-1] != '/' {
		w.Prefix += "/"
	}

	if err := w.setDefaults(); err != nil {
		return err
	}

	stopCh := make(chan bool)
	w.stopChan = stopCh

	errCh := w.ErrChan

	pairCh := make(chan api.KVPairs)

	// The first goroutine we run just sits and waits for updated
	// KVPairs from Consul. This keeps track of the ModifyIndex to use.
	go func() {
		var waitIndex uint64
		for {
			// Setup our variables and query options for the query
			var pairs api.KVPairs
			var meta *api.QueryMeta
			queryOpts := &api.QueryOptions{
				WaitIndex: waitIndex,
				WaitTime:  w.WaitTimeout,
			}

			// Perform a query with exponential backoff to get our pairs
			err := backoff.Retry(func() error {
				// If the quitCh is closed, then just return now and
				// don't make anymore queries to ConsulConnection.
				select {
				case <-stopCh:
					return nil
				default:
				}

				// Query
				var err error
				pairs, meta, err = w.Consul.KV().List(w.Prefix, queryOpts)

				// Been block waiting so first check if stopCh is closed and need to return now
				select {
				case <-stopCh:
					return nil
				default:
				}

				if err != nil {
					errCh <- err
				}

				return err
			}, w.backOffConf())
			if err != nil {
				// These get sent by list as said in mitchellh/consulstructure
				continue
			}

			// Check for quit. If so, quit.
			select {
			case <-stopCh:
				return
			default:
			}

			// If we have the same index, then we didn't find any new values.
			if meta.LastIndex == waitIndex {
				continue
			}

			// Update our wait index
			waitIndex = meta.LastIndex

			// Send the pairs
			pairCh <- pairs
		}
	}()

	// Qsc settings
	qscPeriod := 500 * time.Millisecond
	qscTimeout := 5 * time.Second

	// Listen for pair updates, wait the proper quiesence periods, and
	// trigger configuration updates.
	go func() {
		init := false
		var pairs api.KVPairs
		var qscPeriodCh, qscTimeoutCh <-chan time.Time

		for {
			select {
			case <-stopCh:
				// Exit
				return
			case pairs = <-pairCh:
				// Setup our qsc timers and reloop
				qscPeriodCh = time.After(qscPeriod)
				if qscTimeoutCh == nil {
					qscTimeoutCh = time.After(qscTimeout)
				}

				// If we've initialized already, then we wait for qsc.
				// Otherwise, we go through for the initial config.
				if init {
					continue
				}

				init = true
			case <-qscPeriodCh:
			case <-qscTimeoutCh:
			}

			// Set our timers to nil for the next data
			qscPeriodCh = nil
			qscTimeoutCh = nil

			// Decode and send
			if err := w.process(pairs); err != nil {
				errCh <- err
			}
		}
	}()

	return nil
}

func (w *ConsulWatched) setDefaults() error {
	// Create/use created consul if needed
	if w.Consul == nil {
		err := w.setDefaultConsul()
		if err != nil {
			return err
		}
	}

	// Create requirement channels if needed
	if w.UpdateChan == nil {
		w.UpdateChan = make(chan map[string]interface{})
	}

	if w.ErrChan == nil {
		w.ErrChan = make(chan error)
	}

	// Default wait timeout
	if w.WaitTimeout == 0 {
		w.WaitTimeout = WaitTimeout
	}

	return nil
}

func (w *ConsulWatched) setDefaultConsul() error {
	if len(watchers) > 1 {
		w.Consul = watchers[0].Consul
		watchers = append(watchers, w)
		return nil
	}

	config := api.DefaultConfig()
	config.Address = "127.0.0.1:8500"
	con, err := api.NewClient(config)
	if err != nil {
		return err
	}
	w.Consul = con

	return nil
}

func (w *ConsulWatched) backOffConf() backoff.BackOff {
	conf := backoff.NewExponentialBackOff()
	conf.InitialInterval = 1 * time.Second
	conf.MaxInterval = 10 * time.Second
	conf.MaxElapsedTime = 0
	return conf
}

func (w *ConsulWatched) Stop() {
	w.stopChan <- true
	for i, v := range watchers {
		if v == w {
			watchers[i] = watchers[len(watchers)-1]
			watchers = watchers[:len(watchers)-1]
			return
		}
	}
}

func (w *ConsulWatched) process(pairs api.KVPairs) error {

	var created, deleted map[string]interface{}
	updated := make(map[string]interface{})

	// Split channels if needed
	if w.CreateChan != nil {
		created = make(map[string]interface{})
	} else {
		created = updated
	}
	if w.DeleteChan != nil {
		deleted = make(map[string]interface{})
	} else {
		deleted = updated
	}

	// Create set of existing before processing keys
	for k := range w.updateIndex {
		deleted[k] = nil
	}

	for _, p := range pairs {
		// Trim the Prefix off our key first
		key := strings.TrimPrefix(p.Key, w.Prefix)

		m := created

		// Filter only updated values
		if w.updateIndex != nil {
			// Check if key already processed previously
			if index, exists := w.updateIndex[key]; exists {
				// Delete existing key from deleted set
				delete(deleted, key)

				// Check key index. If modified, when parse
				if index == p.ModifyIndex {
					continue
				} else {
					m = updated
				}
			}

			w.updateIndex[key] = p.ModifyIndex
		}

		m[key] = p.Value
	}

	// Remove deleted entries from watcher list
	for key := range deleted {
		delete(w.updateIndex, key)
	}

	// Send updates
	if len(updated) != 0 {
		w.UpdateChan <- updated
	}
	if w.CreateChan != nil && len(created) != 0 {
		w.CreateChan <- created
	}
	if w.DeleteChan != nil && len(deleted) != 0 {
		w.DeleteChan <- deleted
	}
	return nil
}
