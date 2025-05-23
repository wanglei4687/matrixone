// Copyright 2024 Matrix Origin
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//      http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package v2_0_1

import (
	"fmt"
	"github.com/matrixorigin/matrixone/pkg/bootstrap/versions"
	"github.com/matrixorigin/matrixone/pkg/catalog"
	"github.com/matrixorigin/matrixone/pkg/frontend"
	"github.com/matrixorigin/matrixone/pkg/util/executor"
)

var needMigrateMoPubs = false

var clusterUpgEntries = []versions.UpgradeEntry{
	upg_mo_table_stats,
	upg_mo_pubs_add_account_id_column,
	upg_mo_cdc_task,
	upg_mo_cdc_watermark,
}

var upg_mo_table_stats = versions.UpgradeEntry{
	Schema:    catalog.MO_CATALOG,
	TableName: catalog.MO_TABLE_STATS,
	UpgType:   versions.CREATE_NEW_TABLE,
	UpgSql:    frontend.MoCatalogMoTableStatsDDL,
	CheckFunc: func(txn executor.TxnExecutor, accountId uint32) (bool, error) {
		return versions.CheckTableDefinition(txn, accountId, catalog.MO_CATALOG, catalog.MO_TABLE_STATS)
	},
}

var upg_mo_pubs_add_account_id_column = versions.UpgradeEntry{
	Schema:    catalog.MO_CATALOG,
	TableName: catalog.MO_PUBS,
	UpgType:   versions.ADD_COLUMN,
	UpgSql:    fmt.Sprintf("alter table %s.%s add column account_id int not null first, drop primary key, add primary key(account_id, pub_name)", catalog.MO_CATALOG, catalog.MO_PUBS),
	CheckFunc: func(txn executor.TxnExecutor, accountId uint32) (bool, error) {
		colInfo, err := versions.CheckTableColumn(txn, accountId, catalog.MO_CATALOG, catalog.MO_PUBS, "account_id")
		if err != nil {
			return false, err
		}

		needMigrateMoPubs = !colInfo.IsExits
		return colInfo.IsExits, nil
	},
}

var upg_mo_cdc_task = versions.UpgradeEntry{
	Schema:    catalog.MO_CATALOG,
	TableName: catalog.MO_CDC_TASK,
	UpgType:   versions.CHANGE_COLUMN,
	UpgSql:    fmt.Sprintf("alter table %s.%s change reserved1 err_msg varchar(256)", catalog.MO_CATALOG, catalog.MO_CDC_TASK),
	CheckFunc: func(txn executor.TxnExecutor, accountId uint32) (bool, error) {
		colInfo, err := versions.CheckTableColumn(txn, accountId, catalog.MO_CATALOG, catalog.MO_CDC_TASK, "err_msg")
		if err != nil {
			return false, err
		}
		return colInfo.IsExits, nil
	},
}

var upg_mo_cdc_watermark = versions.UpgradeEntry{
	Schema:    catalog.MO_CATALOG,
	TableName: catalog.MO_CDC_WATERMARK,
	UpgType:   versions.ADD_COLUMN,
	UpgSql: fmt.Sprintf("alter table %s.%s "+
		"add column db_name varchar(256) after table_id, "+
		"add column table_name varchar(256) after db_name, "+
		"add column err_msg varchar(256) after watermark", catalog.MO_CATALOG, catalog.MO_CDC_WATERMARK),
	CheckFunc: func(txn executor.TxnExecutor, accountId uint32) (bool, error) {
		colInfo, err := versions.CheckTableColumn(txn, accountId, catalog.MO_CATALOG, catalog.MO_CDC_WATERMARK, "db_name")
		if err != nil {
			return false, err
		}
		return colInfo.IsExits, nil
	},
}
