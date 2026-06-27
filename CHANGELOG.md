# Changelog

All notable changes to this project are documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

### Changed

- **BREAKING:** user identity is now a username. `OTHERIX_BOOTSTRAP_ADMIN_USERNAME`
  replaces `OTHERIX_BOOTSTRAP_ADMIN_EMAIL` for seeding the first admin, and users
  log in by username. Email is now optional (as is the display name). There is no
  migration path - re-seed the admin (and any users) under the new username-first
  identity.
