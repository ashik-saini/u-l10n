//go:build wireinject
// +build wireinject

package main

import (
	"context"

	"github.com/google/wire"

	scommonconfig "github.com/yougroupteam/s-common-components/config"
	ssecretclient "github.com/yougroupteam/s-common-components/secretclient"
	"github.com/yougroupteam/u-common-components/apm"
	commonconfig "github.com/yougroupteam/u-common-components/config"
	"github.com/yougroupteam/u-common-components/database"
	"github.com/yougroupteam/u-common-components/secretclient"
	storagev4 "github.com/yougroupteam/u-common-components/storage/v4"

	"github.com/yougroupteam/u-l10n/pkg/config"
	"github.com/yougroupteam/u-l10n/pkg/googleauth"
	"github.com/yougroupteam/u-l10n/pkg/repository"
	"github.com/yougroupteam/u-l10n/pkg/service/assetsvc"
	"github.com/yougroupteam/u-l10n/pkg/service/branchsvc"
	"github.com/yougroupteam/u-l10n/pkg/service/exportsvc"
	"github.com/yougroupteam/u-l10n/pkg/service/importsvc"
	"github.com/yougroupteam/u-l10n/pkg/service/keysvc"
	"github.com/yougroupteam/u-l10n/pkg/service/mergesvc"
	"github.com/yougroupteam/u-l10n/pkg/service/mrsvc"
	"github.com/yougroupteam/u-l10n/pkg/service/seed"
	"github.com/yougroupteam/u-l10n/pkg/service/usersvc"
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
	// storage/v4 reads its configuration and credentials through the
	// s-common-components equivalents of the two sets above, which are distinct
	// types from the u-common ones and so must both be present. Same pairing as
	// u-reward's commonWireSet.
	scommonconfig.WireSet,
	ssecretclient.WireSet,
	storagev4.WireSet,
)

func injectService(ctx context.Context) (*Service, error) {
	wire.Build(
		commonWireSet,
		route.WireSet,
		seed.ProvideService,
		exportsvc.ProvideService,
		assetsvc.ProvideService,
		mergesvc.ProvideService,
		importsvc.ProvideService,
		usersvc.ProvideService,
		keysvc.ProvideService,
		branchsvc.ProvideService,
		mrsvc.ProvideService,
		googleauth.ProvideVerifier,
		wire.Struct(new(Service), "*"),
	)
	return nil, nil
}
