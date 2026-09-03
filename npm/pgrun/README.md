# pgrun

Create, use, and delete disposable [PGRun](https://pgrun.dev) Postgres
branches — from the command line or as an MCP tool.

```bash
npx @pgrun/cli auth set --token <TOKEN> --url https://<your-pgrun-host>
npx @pgrun/cli branch create myproject --name scratch --ttl 1h --wait
```

No prior install. `npx` downloads the wrapper and the prebuilt binary for
your platform (shipped as an `optionalDependency`, so npm installs only the
one that matches your OS/CPU) and runs it.

Full documentation: <https://github.com/pgrundev/pgrun-cli#readme>
