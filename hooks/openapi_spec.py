# SPDX-License-Identifier: Apache-2.0
# Copyright 2026 Andrei Taranik

"""MkDocs build hook that publishes the OpenAPI spec as a same-origin docs asset.

The interactive REST API browser (docs/api/index.md, served at /api/) loads the
control-plane spec through Swagger UI. Fetching it cross-origin from
raw.githubusercontent.com fails under the docs site's origin (CORS / CSP), so the
spec is instead served from the docs site itself.

Rather than commit a second copy (which drifts from the source of truth), this
hook copies the canonical api/openapi/control-plane.yaml into docs/api/ on every
build - for both `mkdocs serve` and the CI `mkdocs build`. The copy is gitignored.
"""

import os
import shutil

_ROOT = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
_SRC = os.path.join(_ROOT, "api", "openapi", "control-plane.yaml")
_DST = os.path.join(_ROOT, "docs", "api", "control-plane.yaml")


def on_pre_build(config, **kwargs):
    """Copy the canonical OpenAPI spec next to the REST API browser page."""
    os.makedirs(os.path.dirname(_DST), exist_ok=True)
    shutil.copyfile(_SRC, _DST)
