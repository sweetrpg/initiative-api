#!/bin/bash

set -x
set -e
set -o pipefail

scriptdir="$(cd "$(dirname "${BASH_SOURCE[0]}")" >/dev/null 2>&1 && pwd)"

export FLASK_APP=PACKAGE.application.main:create_app
export FLASK_ENV=development

pushd src

export $(cat ${scriptdir}/../src/configs/local/local.env | xargs)
export NEW_RELIC_CONFIG_FILE=${scriptdir}/../src/configs/local/newrelic.ini

newrelic-admin run-python appserver.py

# $(pyenv which flask) init-db
# $(pyenv which flask) run

popd
