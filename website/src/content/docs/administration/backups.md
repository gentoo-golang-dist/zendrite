---
title: Backups
description: Making backups in Zendrite
---

If you want to keep a backup of your server's data in Zendrite, then you will want to know which data and files to backup. This is also useful if you wanted to transfer to a new server running Zendrite.

## Data to backup

The following is a list of the data you will need to backup:
- matrix-key.pem
- zendrite.yaml
- your server's postgres or sqlite DB
- the jetstream directory
- the media store
- the search index (although this can be regenerated)
