package helper_test

import (
	"context"

	"github.com/marcus13371337/messenger-helper/internal/helper"
)

type fakeMeta struct{}

func newFakeMeta() *fakeMeta { return &fakeMeta{} }

func (f *fakeMeta) connect(ctx context.Context, session helper.Session, emit func(helper.Event)) (helper.Messenger, error) {
	return nil, nil
}
