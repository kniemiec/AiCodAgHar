#!/bin/sh
set -eu
psql -v ON_ERROR_STOP=1 --username "$POSTGRES_USER" <<-EOSQL
    CREATE USER forgejo WITH PASSWORD '${FORGEJO_DB_PASSWORD}';
    CREATE DATABASE forgejo OWNER forgejo;

    CREATE USER orchestrator WITH PASSWORD '${ORCHESTRATOR_DB_PASSWORD}';
    CREATE DATABASE orchestrator OWNER orchestrator;
EOSQL
