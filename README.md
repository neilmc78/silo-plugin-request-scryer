# Silo Scryer Requests

A [Silo](https://github.com/Silo-Server/silo-server) request router that sends approved requests to [Scryer](https://github.com/scryer-media/scryer).

## How it works

1. A request is approved in Silo, and Silo's routing sends it to a Scryer server.
2. The plugin adds the title to Scryer as **monitored** (or reuses it when Scryer already has it) and starts Scryer's automatic search. Scryer's quality profile chooses the release, and its download client fetches it.
3. Every few minutes Silo asks how the request is doing. The plugin reads the title's downloads and files in Scryer:

| Scryer | Silo shows |
|---|---|
| Downloads in flight | Downloading, with progress and an estimated finish |
| Files, nothing downloading | Completed |
| No files, nothing downloading | Searching |
| Unmonitored, no files | Searching, worded "Not monitored" |
| Title removed | Failed |

Silo also completes a request on its own once the title reaches its library.

Series are requested whole; Scryer's monitoring decides which seasons it fetches. Anime series go to Scryer's anime library.

## Setup

1. In Scryer, create an API key under your profile's **API keys**.
2. Install the plugin in Silo (**Admin → Plugins → Upload**) with the `plugin-linux-amd64` binary from a release.
3. Add a request server of type **Scryer** with Scryer's URL (for example `http://scryer:8080`, plus any base path) and the key, and run the connection test.
4. In Silo's request routing, send movies and series to the Scryer server.

## Build

```sh
make build      # ./plugin for this machine
make build-all  # dist/plugin-linux-{amd64,arm64}
make test
```
