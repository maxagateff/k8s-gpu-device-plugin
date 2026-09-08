package src

import (
	"fmt"

	"tags.cncf.io/container-device-interface/pkg/cdi"
)

type CDIResolver interface{ Exists(string) error }
type CDICacheResolver struct{ cache *cdi.Cache }

func NewCDICacheResolver() *CDICacheResolver {
	return &CDICacheResolver{cache: cdi.GetDefaultCache()}
}

func (r *CDICacheResolver) Exists(name string) error {
	_ = r.cache.Refresh()

	if r.cache.GetDevice(name) == nil {
		return fmt.Errorf("CDI device %q not found", name)
	}

	return nil
}
