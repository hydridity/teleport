/*
Copyright 2015-2021 Gravitational, Inc.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package installers

import (
	_ "embed"

	"github.com/gravitational/teleport/api/types"
)

//go:embed installer.sh.tmpl
var defaultInstallScript string

var DefaultInstaller = types.NewInstallerV1(defaultInstallScript)

type InstallerTemplate struct {
	AuthServer string
}