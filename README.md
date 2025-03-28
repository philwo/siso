# philwo/siso

This is my personal, experimental fork of Chromium's Siso build tool and is not
intended for any kind of production use. It is simply a playground for random
ideas and experiments.

The upstream repository for Siso can be found in [Chromium's "infra" repository](https://chromium.googlesource.com/infra/infra/+/refs/heads/main/go/src/infra/build/siso/).

## Rebasing against upstream

The upstream code of Siso can be pulled into the `upstream` branch of this repo
via the included copy.bara.sky config file. It uses Copybara transformations to
only copy the code relevant to Siso from Chromium's `infra` monorepo.

```
$ copybara copy.bara.sky
$ git rebase upstream
```
