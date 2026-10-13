# reMarkable extension

The `remarkable` extension (`extensions/remarkable/`) turns a self-hosted [rmfakecloud](https://github.com/junegunn/rmfakecloud) instance into a document-ingest trigger: it browses the tablet's documents through rmfakecloud's UI API, and dispatches the ones the user explicitly selects into quack's document-ingest workflow. Ingest is user-driven on purpose - every autosave of a note in progress bumps `lastModified`, so anything automatic would run the pipeline against half-written documents.

```yaml
extensions:
  remarkable:
    base_url: https://remarkable.example.duckdns.org   # the rmfakecloud instance
    email: ${REMARKABLE_EMAIL}                         # the rmfakecloud UI account
    password: ${REMARKABLE_PASSWORD}
```

rmfakecloud has no long-lived API token - the extension logs in against `/ui/api/login` with that account and re-logs in on 401. `enabled`/`data_dir`/`exec` are the SDK-level keys every extension block accepts (see the [extensions README](../../extensions/README.md)). The image ships this extension as a child-process binary too: `exec: /quack-ext-remarkable` runs it out of process, with Dispatch and RunEnded crossing the process boundary. A child restart clears every document's in-flight flag although quack's runs continue; each run's RunEnded still settles its document.

The selected documents ride the `document-ingest` workflow shape, which ships in the remarkable plugin (`extensions/remarkable/plugin`, seeded as `github:fagerbergj/quack@plugin/remarkable/vX.Y.Z#extensions/remarkable/plugin`; a `workflows:` entry of the same name in the deployment's config overrides it, see [agents.md](../configuration/agents.md#extending-the-workflow-catalog)) - the extension is the trigger, the classify/index pipeline is the run. Testing without a tablet: `extensions/remarkable/cmd/qa-mock`, covered in [qa-mocks.md](../qa-mocks.md#remarkable-mock).
