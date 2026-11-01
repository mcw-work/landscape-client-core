# Documentation

| Document | Read it for |
|---|---|
| [Architecture](architecture.md) | How the daemon is built: components, goroutines, data flow, state, shutdown |
| [Configuration](configuration.md) | Every config key, the snap hooks, the wizard, and the confdb view |
| [Wire protocol](protocol.md) | bpickle encoding, the exchange and ping requests, registration |
| [Monitoring plugins](monitoring-plugins.md) | What each plugin reports, how often, and from where |
| [Management commands](management-commands.md) | Inbound commands the client executes and the results it returns |
| [Remote script execution](remote-script-execution.md) | The `execute-script` handler in detail |
| [Concurrency](concurrency.md) | Ownership, cancellation and locking rules for contributors |

Historical review notes and delivery plans are kept in [archive/](archive/).
They describe past work, not the current code.
