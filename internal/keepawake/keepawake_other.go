//go:build !windows

package keepawake

const supported = false

type holderImpl struct{}

func newHolderImpl() holderImpl { return holderImpl{} }

func (holderImpl) hold(bool) bool { return false }

func (holderImpl) close() {}
