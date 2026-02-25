// Copyright The OpenTelemetry Authors
// SPDX-License-Identifier: Apache-2.0

package pull // import "github.com/open-telemetry/opentelemetry-collector-contrib/internal/k8sinventory/pull"

import (
	"context"
	"sync"
	"time"

	"go.uber.org/zap"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/tools/pager"

	"github.com/open-telemetry/opentelemetry-collector-contrib/internal/k8sinventory"
)

const defaultPageSize = 500

type Config struct {
	k8sinventory.Config
	Interval time.Duration
	PageSize int
}

type Observer struct {
	config Config

	client dynamic.Interface
	logger *zap.Logger

	handlePullObjectsFunc func(objects *unstructured.UnstructuredList)
}

func New(client dynamic.Interface, config Config, logger *zap.Logger, handlePullObjectsFunc func(objects *unstructured.UnstructuredList)) (*Observer, error) {
	if config.PageSize <= 0 {
		config.PageSize = defaultPageSize
	}
	o := &Observer{
		client:                client,
		config:                config,
		logger:                logger,
		handlePullObjectsFunc: handlePullObjectsFunc,
	}
	return o, nil
}

func (o *Observer) Start(ctx context.Context, wg *sync.WaitGroup) chan struct{} {
	resource := o.client.Resource(o.config.Gvr)
	o.logger.Info("Started collecting",
		zap.Any("gvr", o.config.Gvr),
		zap.Any("mode", "pull"),
		zap.Any("namespaces", o.config.Namespaces))

	stopperChan := make(chan struct{})

	if len(o.config.Namespaces) == 0 {
		wg.Add(1)
		go o.startPull(ctx, resource, stopperChan, wg)
	} else {
		for _, ns := range o.config.Namespaces {
			wg.Add(1)
			go o.startPull(ctx, resource.Namespace(ns), stopperChan, wg)
		}
	}

	return stopperChan
}

func (o *Observer) startPull(ctx context.Context, resource dynamic.ResourceInterface, stopperChan chan struct{}, wg *sync.WaitGroup) {
	defer wg.Done()

	ticker := newTicker(ctx, o.config.Interval)
	listOption := metav1.ListOptions{
		FieldSelector: o.config.FieldSelector,
		LabelSelector: o.config.LabelSelector,
	}

	if o.config.ResourceVersion != "" {
		listOption.ResourceVersion = o.config.ResourceVersion
		listOption.ResourceVersionMatch = metav1.ResourceVersionMatchExact
	}

	p := pager.New(func(ctx context.Context, opts metav1.ListOptions) (runtime.Object, error) {
		return resource.List(ctx, opts)
	})
	p.PageSize = int64(o.config.PageSize)

	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			obj, _, err := p.List(ctx, listOption)
			if err != nil {
				o.logger.Error("error in pulling object",
					zap.String("resource", o.config.Gvr.String()),
					zap.Error(err))
			}
			objects, ok := obj.(*unstructured.UnstructuredList)
			if !ok {
				o.logger.Error("unexpected object type",
					zap.String("resource", o.config.Gvr.String()),
					zap.Any("object", obj))
				continue
			}
			if len(objects.Items) > 0 {
				if o.handlePullObjectsFunc != nil {
					o.handlePullObjectsFunc(objects)
				}
			}
		case <-stopperChan:
			return
		case <-ctx.Done():
			return
		}
	}
}

// Start ticking immediately.
// Ref: https://stackoverflow.com/questions/32705582/how-to-get-time-tick-to-tick-immediately
func newTicker(ctx context.Context, repeat time.Duration) *time.Ticker {
	ticker := time.NewTicker(repeat)
	oc := ticker.C
	nc := make(chan time.Time, 1)
	go func() {
		nc <- time.Now()
		for {
			select {
			case tm := <-oc:
				nc <- tm
			case <-ctx.Done():
				return
			}
		}
	}()

	ticker.C = nc
	return ticker
}
