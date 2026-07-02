# REST API

The Otherix control plane exposes a versioned REST API under `/v1`, consumed by
the `otherix` CLI, the web UI, and your own automation. The full specification is
browsable below.

It is generated from
[`api/openapi/control-plane.yaml`](https://github.com/otherix/otherix/blob/main/api/openapi/control-plane.yaml),
the source of truth, and the spec on this page is published from the exact same
file, so it always matches the version of the docs you are reading. For the
cross-cutting conventions - authentication, pagination, idempotency, async tasks,
and the error envelope - see [API conventions](../reference/api.md).

The interactive viewer is embedded right here - no external tool and no separate
site, it renders the published spec below.

<swagger-ui src="control-plane.yaml"/>

!!! tip "Try it against your own running cluster"
    The viewer above renders the released spec. To explore *your own* control
    plane's live API interactively (issuing real requests), run the bundled
    preview locally:

    ```bash
    make api-preview      # Swagger UI on :8081, Redoc on :8082
    make api-preview-stop
    ```
