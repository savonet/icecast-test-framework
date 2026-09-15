# harbor-audio (liquidsoap) vs harbor-audio (liquidsoap) FRAME=0.2 vs icecast-reference (icecast)

## Summary

| | listeners held | arrivals* | admission p50 / p99 |
|---|---|---|---|
| **harbor-audio (liquidsoap)** | **148k** | 300/s, 10s connect timeout | 315 ms / 9.3 s |
| **harbor-audio (liquidsoap) FRAME=0.2** | **170k** | 300/s, 10s connect timeout | 268 ms / 1.3 s |
| **icecast-reference (icecast)** | **21k** | 60/s, 1m0s connect timeout | 563 ms / 7.6 s |

\* The arrival rate and connect timeout, the deadline for the TCP connect and the HTTP response headers, are the test settings under which the ramp passed, not a measured limit: Icecast could not keep up with the 300 per second the liquidsoap ramps used.

## Setup

### Servers under test

- **harbor-audio (liquidsoap)**: liquidsoap encodes the stream once and serves the listeners itself through output.harbor. (Liquidsoap 2.5.0+git@a079cb9; /mp3 at 128 kbit/s) Measured with default settings, and with FRAME=0.2 (settings.frame.duration, 0.04 s by default).
- **icecast-reference (icecast)**: Reference point: the same liquidsoap encoder feeds Icecast 2.4.4 on the same box through output.icecast, and Icecast serves the listeners. (Liquidsoap 2.5.0+git@5e48837; /mp3 at 128 kbit/s)

### Machines

- server: 8 cpus, 32 GB, 6.12.107+deb13-cloud-amd64 amd64
- 4 load boxes, 3 up to 77046 listeners for harbor-audio (liquidsoap), 3 up to 72000 listeners for harbor-audio (liquidsoap) FRAME=0.2; 8 cpus each

### Scale

- Listeners per step: from 5000 to 172000, the level on the charts, spread evenly over the load boxes; one canary per load box.

### Admission

- How fast new listeners arrive is part of the test and differs by server. **liquidsoap was measured under a surge of 300 new connections per second with a 10s connect timeout; Icecast needed the rate cut to 60 per second and the timeout raised to 1m0s to admit listeners at all.**

### Listeners

- A listener is a TCP connection that sends the HTTP GET a player sends, reads the stream for the whole step and counts the bytes: that is the throughput figure. It does not decode audio; 50% of listeners request ICY metadata and parse every interleaved block.
- A listener fails when it receives nothing for 10s, when the server disconnects it, or when its rate over the last 10 s lags the median of its mount by more than 20% for 5 s in a row.

### Canaries

- Decoding is checked by an ffmpeg process per load box, mount and step. Each joins the stream mid-way and must decode 10s of it without error, as a player joining a running stream would.

### Pass rules

- Each level is held for 1m0s once reached.
- A step passes when fewer than 1.0% of its listeners fail, the median listener receives at least 90% of the nominal bitrate, every canary decodes, and the server logs no alert.

## Results

| run | enabled | ceiling | Mbit/s at ceiling | server cores at ceiling | server RSS at ceiling | ttfb p50 / p99 at ceiling | range |
|---|---|---|---|---|---|---|---|
| harbor-audio (liquidsoap) | MP3 | 148000 | 18953 | 7.44 | 390 MB | 315 / 9276 ms | 5001 to 150000, hold 1m0s |
| harbor-audio (liquidsoap) FRAME=0.2 | MP3 | 170000 | 21677 | 7.23 | 419 MB | 268 / 1287 ms | 30000 to 172000, hold 1m0s |
| icecast-reference (icecast) | MP3 | 21000 | 2687 | 0.90 | 53 MB | 563 / 7561 ms | 5000 to 22014, hold 1m0s |

## Reading the results

### Icecast serves from one core, liquidsoap from all of them

- Icecast 2.4 serves a mount from a single thread, which reads the stream, writes to every listener and admits new ones in one loop (`source_main` in `src/source.c`); a thread runs on one core at a time, so the process tops out at 1.00 core on a box that is 15% busy, and the spread bars below only show the scheduler moving it around.
- liquidsoap serves a mount from eight writer tasks on OCaml 5 domains, which are 7.2 cores at 170k on a box that is 99% busy, every core at 98 to 99%.
- The work itself costs the same on both: server CPU divided by listeners converges on about 50 µs per listener per second for all three series, Icecast 47 µs, liquidsoap 43 to 55 µs at the 0.2 s frame and 217 µs falling to 50 µs at the default frame.
- That cost is the kernel's TCP send path, which neither server can avoid.
- The 8× is parallelism, not a cheaper per-listener path, and on this box it runs into the NIC before the cores: 22.2 of 23 Gbit/s at 170k.

### The 0.2 s frame is the better setting for a large audience

- A listener gets one write per frame, so `settings.frame.duration := 0.2` issues five writes per listener per second instead of 25 and drops the per-write overhead: 41% fewer cores at 60k, 10% at 148k.
- With that overhead gone the server reaches the link before the cores: 170000 listeners at 22.2 of 23 Gbit/s, where the default frame stops at 148000 with the cores full.
- It also admits faster at the top: 1.3 s at p99 at 170k, against 9 s for the default frame at 148k.
- The price is 0.2 s of delay between the encoder and every listener, which is negligible next to the burst a listener receives on connect: 64 KB, about four seconds of stream at 128 kbit/s, which already puts every listener seconds behind live.
- On a wider link the next limit is CPU, about 185k listeners on this box at its 43 µs per listener.

### Admission

- liquidsoap accepts on its harbor thread, apart from the writers: median time to first byte stays under 120 ms up to 120k at either frame.
- Icecast admits on the thread that writes to the existing listeners: p99 4.3 s at 19k and 7.6 s at 21k, connection errors at 22k.
- Continuous arrivals at 300 per second carried the default frame to 78k but not to 124k, where the 0.2 s frame passed.
- At 750 and 1200 arrivals per second the default frame collapsed at 66k and 61k.

## Server machine at the ceiling

| run | busy per core | user / system / interrupts | memory used | socket buffers | NIC out | packets out /s | retransmits /s |
|---|---|---|---|---|---|---|---|
| harbor-audio (liquidsoap) FRAME=0.2 | 99% 99% 99% 99% 98% 99% 99% 99% | 15% / 32% / 52% | 2141 MB | 170 MB | 22169 Mbit/s | 820395 | 33291 |
| icecast-reference (icecast) | 11% 27% 15% 3% 11% 18% 30% 3% | 3% / 9% / 3% | 2013 MB | 5 MB | 2820 Mbit/s | 250440 | 0 |

## Steps

| listeners | harbor-audio (liquidsoap): ok | Mbit/s | ttfb p99 ms | server cores | server MB | failures | harbor-audio (liquidsoap) FRAME=0.2: ok | Mbit/s | ttfb p99 ms | server cores | server MB | failures | icecast-reference (icecast): ok | Mbit/s | ttfb p99 ms | server cores | server MB | failures |
|---|---|---|---|---|---|---|---|---|---|---|---|---|---|---|---|---|---|---|
| 5000 | | | | | | | | | | | | | pass | 640 | 403 | 0.20 | 25 | 0 |
| 5001 | pass | 640 | 12 | 1.09 | 214 | 0 | | | | | | | | | | | | |
| 7000 | | | | | | | | | | | | | pass | 897 | 402 | 0.30 | 28 | 0 |
| 7503 | pass | 961 | 17 | 1.28 | 220 | 0 | | | | | | | | | | | | |
| 9000 | | | | | | | | | | | | | pass | 1152 | 414 | 0.37 | 32 | 0 |
| 10005 | pass | 1282 | 24 | 1.50 | 226 | 0 | | | | | | | | | | | | |
| 11000 | | | | | | | | | | | | | pass | 1408 | 425 | 0.47 | 35 | 0 |
| 12507 | pass | 1602 | 28 | 1.74 | 230 | 0 | | | | | | | | | | | | |
| 13000 | | | | | | | | | | | | | pass | 1664 | 756 | 0.56 | 39 | 0 |
| 15000 | | | | | | | | | | | | | pass | 1923 | 519 | 0.66 | 42 | 0 |
| 15009 | pass | 1921 | 30 | 1.92 | 234 | 0 | | | | | | | | | | | | |
| 17000 | | | | | | | | | | | | | pass | 2177 | 1326 | 0.73 | 46 | 0 |
| 17511 | pass | 2242 | 34 | 2.10 | 239 | 0 | | | | | | | | | | | | |
| 19000 | | | | | | | | | | | | | pass | 2432 | 4337 | 0.82 | 49 | 0 |
| 20013 | pass | 2562 | 37 | 2.34 | 242 | 0 | | | | | | | | | | | | |
| 21000 | | | | | | | | | | | | | pass | 2687 | 7561 | 0.90 | 53 | 0 |
| 22014 | | | | | | | | | | | | | FAIL | 2914 | 58225 | 1.00 | 55 | 760 |
| 22515 | pass | 2882 | 41 | 2.61 | 245 | 0 | | | | | | | | | | | | |
| 25017 | pass | 3203 | 41 | 2.79 | 251 | 0 | | | | | | | | | | | | |
| 27519 | pass | 3521 | 43 | 2.90 | 254 | 0 | | | | | | | | | | | | |
| 30000 | pass | 3741 | 51 | 3.16 | 258 | 0 | pass | 3840 | 47 | 1.57 | 253 | 0 | | | | | | |
| 36000 | pass | 4610 | 1081 | 3.69 | 264 | 0 | pass | 4609 | 67 | 1.91 | 266 | 0 | | | | | | |
| 42000 | pass | 5381 | 1097 | 4.21 | 273 | 0 | pass | 5376 | 77 | 2.24 | 275 | 0 | | | | | | |
| 48000 | pass | 6146 | 1049 | 4.71 | 280 | 0 | pass | 6145 | 85 | 2.55 | 285 | 0 | | | | | | |
| 54000 | pass | 6917 | 1125 | 5.23 | 289 | 0 | pass | 6912 | 90 | 2.90 | 291 | 0 | | | | | | |
| 55002 | pass | 7045 | 83 | 5.30 | 266 | 0 | | | | | | | | | | | | |
| 56004 | pass | 7172 | 1071 | 5.35 | 283 | 0 | | | | | | | | | | | | |
| 57006 | pass | 7298 | 1084 | 5.38 | 284 | 0 | | | | | | | | | | | | |
| 58008 | pass | 7430 | 1070 | 5.40 | 284 | 0 | | | | | | | | | | | | |
| 59010 | pass | 7555 | 1098 | 5.50 | 285 | 0 | | | | | | | | | | | | |
| 60000 | | | | | | | pass | 7681 | 95 | 3.24 | 298 | 0 | | | | | | |
| 60012 | pass | 7680 | 1122 | 5.51 | 285 | 0 | | | | | | | | | | | | |
| 61014 | pass | 7810 | 1121 | 5.65 | 285 | 0 | | | | | | | | | | | | |
| 62016 | pass | 7936 | 1141 | 5.76 | 287 | 0 | | | | | | | | | | | | |
| 63018 | pass | 8070 | 1077 | 5.80 | 287 | 0 | | | | | | | | | | | | |
| 64020 | pass | 8197 | 1080 | 5.81 | 290 | 0 | | | | | | | | | | | | |
| 65022 | pass | 8324 | 1161 | 5.91 | 291 | 0 | | | | | | | | | | | | |
| 66000 | | | | | | | pass | 8451 | 116 | 3.58 | 305 | 0 | | | | | | |
| 66024 | pass | 8453 | 1124 | 6.01 | 292 | 0 | | | | | | | | | | | | |
| 67026 | pass | 8579 | 1094 | 6.14 | 292 | 0 | | | | | | | | | | | | |
| 68028 | pass | 8711 | 1150 | 6.36 | 294 | 0 | | | | | | | | | | | | |
| 69030 | pass | 8836 | 1122 | 6.38 | 295 | 0 | | | | | | | | | | | | |
| 70032 | pass | 8969 | 1163 | 6.34 | 296 | 0 | | | | | | | | | | | | |
| 71034 | pass | 9091 | 1141 | 6.46 | 299 | 0 | | | | | | | | | | | | |
| 72000 | | | | | | | pass | 9215 | 122 | 3.89 | 312 | 0 | | | | | | |
| 72036 | pass | 9223 | 1230 | 6.58 | 301 | 0 | | | | | | | | | | | | |
| 73038 | pass | 9350 | 1153 | 6.62 | 301 | 0 | | | | | | | | | | | | |
| 74040 | pass | 9479 | 1198 | 6.71 | 301 | 0 | | | | | | | | | | | | |
| 75042 | pass | 9612 | 1068 | 6.79 | 303 | 0 | | | | | | | | | | | | |
| 76044 | pass | 9735 | 1165 | 6.83 | 304 | 0 | | | | | | | | | | | | |
| 77046 | pass | 9865 | 1171 | 6.88 | 307 | 0 | | | | | | | | | | | | |
| 78000 | pass | 9984 | 118 | 7.06 | 310 | 0 | pass | 9985 | 123 | 4.30 | 320 | 0 | | | | | | |
| 80000 | pass | 10241 | 364 | 7.10 | 313 | 0 | pass | 10236 | 152 | 4.48 | 321 | 0 | | | | | | |
| 82000 | pass | 10498 | 289 | 7.12 | 315 | 0 | pass | 10495 | 173 | 5.00 | 322 | 0 | | | | | | |
| 84000 | pass | 10755 | 249 | 7.14 | 317 | 0 | pass | 10752 | 160 | 5.33 | 325 | 0 | | | | | | |
| 86000 | pass | 11006 | 510 | 7.17 | 320 | 0 | pass | 11013 | 171 | 5.22 | 326 | 0 | | | | | | |
| 88000 | pass | 11267 | 470 | 7.36 | 322 | 0 | pass | 11266 | 147 | 4.55 | 327 | 0 | | | | | | |
| 90000 | pass | 11524 | 619 | 7.21 | 324 | 0 | pass | 11524 | 158 | 4.57 | 330 | 0 | | | | | | |
| 92000 | pass | 11779 | 731 | 7.38 | 325 | 0 | pass | 11778 | 147 | 4.65 | 331 | 0 | | | | | | |
| 94000 | pass | 12037 | 597 | 7.42 | 328 | 0 | pass | 12029 | 154 | 4.72 | 335 | 0 | | | | | | |
| 96000 | pass | 12289 | 438 | 7.43 | 329 | 0 | pass | 12289 | 177 | 4.95 | 336 | 0 | | | | | | |
| 98000 | pass | 12549 | 760 | 7.39 | 332 | 0 | pass | 12541 | 165 | 4.92 | 337 | 0 | | | | | | |
| 100000 | pass | 12801 | 326 | 7.44 | 334 | 0 | pass | 12798 | 163 | 4.98 | 339 | 0 | | | | | | |
| 102000 | pass | 13062 | 1248 | 7.48 | 337 | 0 | pass | 13058 | 177 | 4.99 | 342 | 0 | | | | | | |
| 104000 | pass | 13315 | 389 | 7.47 | 338 | 0 | pass | 13313 | 180 | 5.17 | 344 | 0 | | | | | | |
| 106000 | pass | 13568 | 318 | 7.51 | 339 | 0 | pass | 13568 | 167 | 5.33 | 345 | 0 | | | | | | |
| 108000 | pass | 13824 | 684 | 7.51 | 342 | 0 | pass | 13829 | 174 | 5.29 | 346 | 0 | | | | | | |
| 110000 | pass | 14083 | 879 | 7.30 | 343 | 0 | pass | 14081 | 171 | 5.45 | 350 | 0 | | | | | | |
| 112000 | pass | 14340 | 1313 | 7.50 | 344 | 0 | pass | 14333 | 191 | 5.50 | 350 | 0 | | | | | | |
| 114000 | pass | 14593 | 2846 | 7.50 | 346 | 0 | pass | 14595 | 191 | 5.63 | 354 | 0 | | | | | | |
| 116000 | pass | 14847 | 1369 | 7.50 | 347 | 0 | pass | 14850 | 172 | 5.81 | 356 | 0 | | | | | | |
| 118000 | pass | 15106 | 422 | 7.51 | 349 | 0 | pass | 15106 | 189 | 5.96 | 358 | 0 | | | | | | |
| 120000 | pass | 15361 | 1026 | 7.50 | 354 | 0 | pass | 15361 | 185 | 6.03 | 359 | 0 | | | | | | |
| 122000 | pass | 15618 | 813 | 7.48 | 354 | 0 | pass | 15622 | 352 | 5.89 | 362 | 0 | | | | | | |
| 124000 | pass | 15869 | 805 | 7.49 | 356 | 0 | pass | 15870 | 196 | 6.13 | 364 | 0 | | | | | | |
| 126000 | pass | 16134 | 924 | 7.46 | 358 | 0 | pass | 16130 | 593 | 6.24 | 367 | 0 | | | | | | |
| 128000 | pass | 16379 | 917 | 7.48 | 360 | 0 | pass | 16386 | 598 | 6.18 | 368 | 0 | | | | | | |
| 130000 | pass | 16644 | 1658 | 7.47 | 362 | 0 | pass | 16639 | 362 | 6.16 | 371 | 0 | | | | | | |
| 132000 | pass | 16899 | 897 | 7.47 | 365 | 0 | pass | 16902 | 206 | 6.20 | 371 | 0 | | | | | | |
| 134000 | pass | 17158 | 845 | 7.47 | 366 | 0 | pass | 17154 | 607 | 6.20 | 375 | 0 | | | | | | |
| 136000 | pass | 17413 | 1553 | 7.47 | 369 | 0 | pass | 17407 | 585 | 6.22 | 375 | 0 | | | | | | |
| 138000 | pass | 17664 | 6542 | 7.46 | 370 | 0 | pass | 17666 | 404 | 6.28 | 382 | 0 | | | | | | |
| 140000 | pass | 17923 | 7143 | 7.46 | 373 | 0 | pass | 17922 | 363 | 6.33 | 382 | 0 | | | | | | |
| 142000 | pass | 18176 | 8960 | 7.44 | 382 | 297 | pass | 18180 | 971 | 6.39 | 384 | 0 | | | | | | |
| 144000 | pass | 18434 | 8935 | 7.46 | 383 | 0 | pass | 18433 | 768 | 6.58 | 386 | 0 | | | | | | |
| 146000 | pass | 18698 | 8907 | 7.45 | 389 | 349 | pass | 18692 | 969 | 6.66 | 390 | 0 | | | | | | |
| 148000 | pass | 18953 | 9276 | 7.44 | 390 | 219 | pass | 18944 | 415 | 6.70 | 390 | 0 | | | | | | |
| 150000 | FAIL | 16495 | 13776 | 5.56 | 533 | 18896 | pass | 19207 | 380 | 6.74 | 393 | 0 | | | | | | |
| 152000 | | | | | | | pass | 19461 | 609 | 6.82 | 395 | 0 | | | | | | |
| 154000 | | | | | | | pass | 19715 | 1856 | 6.86 | 395 | 0 | | | | | | |
| 156000 | | | | | | | pass | 19973 | 774 | 6.97 | 398 | 0 | | | | | | |
| 158000 | | | | | | | pass | 20229 | 461 | 7.04 | 400 | 0 | | | | | | |
| 160000 | | | | | | | pass | 20481 | 1243 | 7.18 | 403 | 0 | | | | | | |
| 162000 | | | | | | | pass | 20735 | 498 | 7.29 | 409 | 0 | | | | | | |
| 164000 | | | | | | | pass | 20993 | 593 | 7.38 | 412 | 0 | | | | | | |
| 166000 | | | | | | | pass | 21254 | 949 | 7.36 | 412 | 0 | | | | | | |
| 168000 | | | | | | | pass | 21500 | 1079 | 7.30 | 413 | 0 | | | | | | |
| 170000 | | | | | | | pass | 21677 | 1287 | 7.23 | 419 | 0 | | | | | | |
| 172000 | | | | | | | FAIL | 21717 | 1943 | 6.93 | 443 | 0 | | | | | | |
