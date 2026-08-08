//go:build wireinject
// +build wireinject

package main

import (
	"context"

	"github.com/google/wire"

	"github.com/yougroupteam/u-common-components/apm"
	commonconfig "github.com/yougroupteam/u-common-components/config"
	"github.com/yougroupteam/u-common-components/database"
	"github.com/yougroupteam/u-common-components/secretclient"

	"github.com/yougroupteam/u-l10n/pkg/config"
	"github.com/yougroupteam/u-l10n/pkg/repository"
	"github.com/yougroupteam/u-l10n/pkg/service/seed"
	"github.com/yougroupteam/u-l10n/route"
)

// commonWireSet holds the shared infrastructure providers. Feature packages
// get their own sets so this one stays the boring, stable part of the graph.
var commonWireSet = wire.NewSet(
	commonconfig.WireSet,
	apm.WireSet,
	secretclient.WireSet,
	database.WireSet,
	database.ProvideTransactional,
	config.ProvideConfig,
	repository.WireSet,
)

func injectService(ctx context.Context) (*Service, error) {
	wire.Build(
		commonWireSet,
		route.WireSet,
		seed.ProvideService,
		wire.Struct(new(Service), "*"),
	)
	return nil, nil
}
