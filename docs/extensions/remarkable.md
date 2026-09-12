# reMarkable extension

The `remarkable` extension (in [quack-extensions](https://github.com/fagerbergj/quack-extensions), `remarkable/`) turns a self-hosted [rmfakecloud](https://github.com/junegunn/rmfakecloud) instance into a document-ingest trigger: it browses the tablet's documents through rmfakecloud's UI API, and dispatches the ones the user explicitly selects into quack's document-ingest workflow. Ingest is user-driven on purpose - every autosave of a note in progress bumps `lastModified`, so anything automatic would run the pipeline against half-written documents.

```yaml
extensions:
  remarkable:
    base_url: https://remarkable.example.duckdns.org   # the rmfakecloud instance
    email: ${REMARKABLE_EMAIL}                         # the rmfakecloud UI account
    password: ${REMARKABLE_PASSWORD}
```

rmfakecloud has no long-lived API token - the extension logs in against `/ui/api/login` with that account and re-logs in on 401. `enabled`/`data_dir` are the SDK-level keys every extension block accepts (see the [quack-extensions README](https://github.com/fagerbergj/quack-extensions)).

The selected documents ride the `document-ingest` workflow shape (see [agents.md](../configuration/agents.md#extending-the-workflow-catalog)) - the extension is the trigger, the classify/index pipeline is the run. Testing without a tablet: `remarkable/cmd/qa-mock`, covered in [qa-mocks.md](../qa-mocks.md#remarkable-mock).
