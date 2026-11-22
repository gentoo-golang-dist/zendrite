---
title: Preparing database storage
description: Setting up PostgreSQL or SQLite databases for Zendrite
---

Zendrite stores its data in an SQL database.
PostgreSQL needs a role and a database that you create by hand.
SQLite needs no manual steps.
After this, [generate a signing key](/installation/manual/signingkey).

## PostgreSQL

Zendrite creates its own tables and indexes, but it can't create the database itself.
Create the database by hand with UTF-8 encoding, or your deployment will likely run into problems.

Create a single PostgreSQL database.
Use a single global connection pool, which makes the configuration file easier to update.
You then manage one connection string and have one database to back up.
All components share the same database resources (CPU, RAM, and storage).

You need to:

1. [Create a role](#role-creation) with a username and password that Zendrite uses to connect.
2. [Create the database](#single-database-creation) and give the Zendrite role privileges over it.
   Zendrite creates and manages its own tables, indexes, and sequences, so the role needs suitable privileges on the database.

### Role creation

Create a role for Zendrite and choose a new password when prompted.
On macOS, you may need to omit `sudo -u postgres` from the following commands.

```bash
sudo -u postgres createuser -P zendrite
```

### Single database creation

Create the database, owned by the `zendrite` role:

```bash
sudo -u postgres createdb -O zendrite -E UTF-8 zendrite
```

### Connection strings

The [PostgreSQL libpq manual](https://www.postgresql.org/docs/current/libpq-connect.html#LIBPQ-CONNSTRING) describes the connection string format.
Zendrite supports only the "Connection URIs" format and **doesn't** work with the "Keyword/Value Connection string" format.

Supported connection strings look like this:

- `postgresql://user:pass@hostname/database?options=...`
- `postgres://user:pass@hostname/database?options=...`

To disable SSL/TLS on the database connection, you may need to append `?sslmode=disable` to the connection string.

If Zendrite and PostgreSQL run on the same Linux machine and the system username matches the PostgreSQL username, connect through a UNIX socket.
This is more efficient (no SSL) and more secure (no password).
For example, for a database called `zendritedb`:

- `postgresql:///zendritedb?host=/run/postgresql`

## SQLite

**WARNING:** The Zendrite SQLite backend is slower, less reliable, and not recommended for production.
Use PostgreSQL instead.
We may not be able to help if you run into issues with the SQLite backend.

Set the database filenames in the Zendrite configuration file and start Zendrite.
Zendrite creates and populates the databases automatically.

Zendrite **can't share a single SQLite database across multiple components**.
Give each component its own SQLite database filename.
Remove the `global.database` section from your Zendrite config and add a `database` section to each component instead.

### Connection strings

SQLite connection strings take these forms:

- Path relative to the current working directory: `file:zendrite_component.db`
- Full path: `file:///path/to/zendrite_component.db`
