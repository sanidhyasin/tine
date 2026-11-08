# Results: m8i.xlarge, nested virtualization

| Host | |
|---|---|
| Instance | m8i.xlarge |
| Virtualization | amazon |
| CPU | Intel(R) Xeon(R) 6975P-C |
| CPUs | 4 |
| Memory (MiB) | 15701 |
| Kernel | 7.0.0-1014-aws |
| Firecracker | Firecracker v1.17.0 |
| tine | v0.3.0-15-g86ad1cb |
| Data disk | nvme1n1: Amazon Elastic Block Store |
| vm.dirty_ratio | 60 |
| THP | madvise |
| KSM | 0 |

Times are in milliseconds. Warm-up and failed runs are left out; how many is noted.

## create-cold

| params | measure | n | p50 | p95 | p99 | min | max |
|---|---|---|---|---|---|---|---|
| features=thp, memory_mib=1024, vcpus=2 | total | 100 | 1644.4 | 1702.2 | 1723.4 | 1532.3 | 1731.7 |
| features=thp, memory_mib=1024, vcpus=2 | client | 100 | 1644.9 | 1702.7 | 1724.5 | 1533.0 | 1732.2 |
| features=thp, memory_mib=1024, vcpus=2 | destroy | 100 | 25.0 | 32.9 | 34.8 | 18.5 | 40.5 |
| memory_mib=1024, vcpus=2 | total | 100 | 1831.9 | 1906.7 | 1931.3 | 1696.8 | 1951.0 |
| memory_mib=1024, vcpus=2 | client | 100 | 1832.5 | 1907.9 | 1931.8 | 1697.3 | 1951.6 |
| memory_mib=1024, vcpus=2 | destroy | 100 | 31.3 | 39.0 | 46.2 | 24.7 | 50.3 |

## create-template

| params | measure | n | p50 | p95 | p99 | min | max |
|---|---|---|---|---|---|---|---|
| memory_mib=1024, strategy=copy, vcpus=2 | total | 100 | 338.6 | 349.7 | 356.0 | 325.5 | 362.3 |
| memory_mib=1024, strategy=copy, vcpus=2 | client | 100 | 339.2 | 350.3 | 356.5 | 326.1 | 362.8 |
| memory_mib=1024, strategy=copy, vcpus=2 | destroy | 100 | 28.4 | 35.0 | 38.9 | 20.9 | 52.5 |
| memory_mib=1024, strategy=uffd, vcpus=2 | total | 100 | 77.5 | 85.6 | 97.6 | 69.4 | 99.8 |
| memory_mib=1024, strategy=uffd, vcpus=2 | client | 100 | 78.1 | 86.9 | 98.3 | 69.8 | 100.5 |
| memory_mib=1024, strategy=uffd, vcpus=2 | destroy | 100 | 40.2 | 49.8 | 53.9 | 7.1 | 54.0 |
| memory_mib=1024, strategy=uffd-prefetch, vcpus=2 | total | 100 | 41.0 | 46.1 | 54.8 | 37.0 | 57.1 |
| memory_mib=1024, strategy=uffd-prefetch, vcpus=2 | client | 100 | 41.4 | 46.6 | 55.2 | 37.4 | 57.5 |
| memory_mib=1024, strategy=uffd-prefetch, vcpus=2 | destroy | 100 | 33.3 | 50.6 | 57.3 | 14.0 | 60.6 |
| memory_mib=1024, vcpus=2 | total | 100 | 56.5 | 64.4 | 68.8 | 52.5 | 77.9 |
| memory_mib=1024, vcpus=2 | client | 100 | 57.0 | 66.2 | 71.1 | 52.9 | 78.4 |
| memory_mib=1024, vcpus=2 | destroy | 100 | 25.5 | 34.7 | 38.4 | 16.7 | 39.7 |

## density

- `{'memory_mib': 256, 'vcpus': 1}`: child_pss (MiB) 20.8, child_rss (MiB) 103.0, children 31.0, host_available (MiB) 14763.0, parent_pss (MiB) 109.0, parent_rss (MiB) 109.0, seconds 9.4

## exec

| params | measure | n | p50 | p95 | p99 | min | max |
|---|---|---|---|---|---|---|---|
| command=true | client | 200 | 1.9 | 2.9 | 4.7 | 1.4 | 7.4 |
| command=true | guest | 200 | 1.1 | 1.9 | 2.8 | 0.8 | 4.2 |

## fork

| params | measure | n | p50 | p95 | p99 | min | max |
|---|---|---|---|---|---|---|---|
| children=1, dirty_mib=0, features=diff-snapshots, memory_mib=256, strategy=share, vcpus=1 | total | 10 | 3215.2 | 3223.7 | 3223.7 | 3204.7 | 3223.7 |
| children=1, dirty_mib=0, features=diff-snapshots, memory_mib=256, strategy=share, vcpus=1 | paused | 10 | 7.1 | 8.1 | 8.1 | 7.0 | 8.1 |
| children=1, dirty_mib=0, features=diff-snapshots, memory_mib=256, strategy=share, vcpus=1 | children | 10 | 85.9 | 93.6 | 93.6 | 75.0 | 93.6 |
| children=1, dirty_mib=0, features=diff-snapshots, memory_mib=256, strategy=share, vcpus=1 | child_p50 | 10 | 85.7 | 93.4 | 93.4 | 74.8 | 93.4 |
| children=1, dirty_mib=0, memory_mib=256, strategy=copy, vcpus=1 | total | 10 | 3259.1 | 3280.9 | 3280.9 | 3253.6 | 3280.9 |
| children=1, dirty_mib=0, memory_mib=256, strategy=copy, vcpus=1 | paused | 10 | 49.3 | 50.4 | 50.4 | 48.9 | 50.4 |
| children=1, dirty_mib=0, memory_mib=256, strategy=copy, vcpus=1 | children | 10 | 157.8 | 179.2 | 179.2 | 151.3 | 179.2 |
| children=1, dirty_mib=0, memory_mib=256, strategy=copy, vcpus=1 | child_p50 | 10 | 157.6 | 179.0 | 179.0 | 151.1 | 179.0 |
| children=1, dirty_mib=0, memory_mib=256, strategy=share, vcpus=1 | total | 10 | 3175.4 | 3178.6 | 3178.6 | 3172.9 | 3178.6 |
| children=1, dirty_mib=0, memory_mib=256, strategy=share, vcpus=1 | paused | 10 | 49.3 | 49.7 | 49.7 | 49.1 | 49.7 |
| children=1, dirty_mib=0, memory_mib=256, strategy=share, vcpus=1 | children | 10 | 74.2 | 76.6 | 76.6 | 71.6 | 76.6 |
| children=1, dirty_mib=0, memory_mib=256, strategy=share, vcpus=1 | child_p50 | 10 | 74.0 | 76.3 | 76.3 | 71.3 | 76.3 |
| children=1, dirty_mib=64, features=diff-snapshots, memory_mib=256, strategy=share, vcpus=1 | total | 10 | 3203.5 | 3215.4 | 3215.4 | 3194.8 | 3215.4 |
| children=1, dirty_mib=64, features=diff-snapshots, memory_mib=256, strategy=share, vcpus=1 | paused | 10 | 22.4 | 25.4 | 25.4 | 22.2 | 25.4 |
| children=1, dirty_mib=64, features=diff-snapshots, memory_mib=256, strategy=share, vcpus=1 | children | 10 | 86.0 | 93.4 | 93.4 | 80.4 | 93.4 |
| children=1, dirty_mib=64, features=diff-snapshots, memory_mib=256, strategy=share, vcpus=1 | child_p50 | 10 | 85.8 | 93.2 | 93.2 | 80.1 | 93.2 |
| children=1, dirty_mib=64, memory_mib=256, strategy=copy, vcpus=1 | total | 10 | 3251.2 | 3260.9 | 3260.9 | 3240.9 | 3260.9 |
| children=1, dirty_mib=64, memory_mib=256, strategy=copy, vcpus=1 | paused | 10 | 54.0 | 54.2 | 54.2 | 53.6 | 54.2 |
| children=1, dirty_mib=64, memory_mib=256, strategy=copy, vcpus=1 | children | 10 | 157.8 | 167.9 | 167.9 | 147.2 | 167.9 |
| children=1, dirty_mib=64, memory_mib=256, strategy=copy, vcpus=1 | child_p50 | 10 | 157.6 | 167.5 | 167.5 | 146.9 | 167.5 |
| children=1, dirty_mib=64, memory_mib=256, strategy=share, vcpus=1 | total | 10 | 3168.4 | 3174.2 | 3174.2 | 3161.2 | 3174.2 |
| children=1, dirty_mib=64, memory_mib=256, strategy=share, vcpus=1 | paused | 10 | 53.8 | 54.9 | 54.9 | 53.6 | 54.9 |
| children=1, dirty_mib=64, memory_mib=256, strategy=share, vcpus=1 | children | 10 | 74.3 | 80.1 | 80.1 | 67.1 | 80.1 |
| children=1, dirty_mib=64, memory_mib=256, strategy=share, vcpus=1 | child_p50 | 10 | 74.1 | 79.9 | 79.9 | 66.8 | 79.9 |
| children=1, features=diff-snapshots, memory_mib=256, strategy=share, vcpus=1 | total | 10 | 152.7 | 157.3 | 157.3 | 148.9 | 157.3 |
| children=1, features=diff-snapshots, memory_mib=256, strategy=share, vcpus=1 | paused | 10 | 64.3 | 65.2 | 65.2 | 63.8 | 65.2 |
| children=1, features=diff-snapshots, memory_mib=256, strategy=share, vcpus=1 | children | 10 | 82.1 | 86.7 | 86.7 | 78.8 | 86.7 |
| children=1, features=diff-snapshots, memory_mib=256, strategy=share, vcpus=1 | child_p50 | 10 | 80.5 | 85.1 | 85.1 | 78.5 | 85.1 |
| children=1, features=huge-pages-2M, memory_mib=256, strategy=uffd, vcpus=1 | total | 10 | 119.5 | 131.1 | 131.1 | 115.4 | 131.1 |
| children=1, features=huge-pages-2M, memory_mib=256, strategy=uffd, vcpus=1 | paused | 10 | 63.2 | 63.8 | 63.8 | 62.9 | 63.8 |
| children=1, features=huge-pages-2M, memory_mib=256, strategy=uffd, vcpus=1 | children | 10 | 53.3 | 64.8 | 64.8 | 49.9 | 64.8 |
| children=1, features=huge-pages-2M, memory_mib=256, strategy=uffd, vcpus=1 | child_p50 | 10 | 53.1 | 62.7 | 62.7 | 49.7 | 62.7 |
| children=1, features=huge-pages-2M, memory_mib=256, strategy=uffd-prefetch, vcpus=1 | total | 10 | 117.6 | 122.2 | 122.2 | 115.0 | 122.2 |
| children=1, features=huge-pages-2M, memory_mib=256, strategy=uffd-prefetch, vcpus=1 | paused | 10 | 63.2 | 64.0 | 64.0 | 62.9 | 64.0 |
| children=1, features=huge-pages-2M, memory_mib=256, strategy=uffd-prefetch, vcpus=1 | first | 10 | 51.3 | 56.0 | 56.0 | 49.7 | 56.0 |
| children=1, features=huge-pages-2M, memory_mib=256, strategy=uffd-prefetch, vcpus=1 | children | 10 | 0.0 | 0.0 | 0.0 | 0.0 | 0.0 |
| children=1, features=huge-pages-2M, memory_mib=256, strategy=uffd-prefetch, vcpus=1 | child_p50 | 10 | 51.0 | 55.7 | 55.7 | 49.4 | 55.7 |
| children=1, features=thp, memory_mib=256, strategy=share, vcpus=1 | total | 10 | 126.2 | 133.4 | 133.4 | 123.4 | 133.4 |
| children=1, features=thp, memory_mib=256, strategy=share, vcpus=1 | paused | 10 | 53.8 | 54.3 | 54.3 | 53.3 | 54.3 |
| children=1, features=thp, memory_mib=256, strategy=share, vcpus=1 | children | 10 | 70.1 | 77.2 | 77.2 | 67.6 | 77.2 |
| children=1, features=thp, memory_mib=256, strategy=share, vcpus=1 | child_p50 | 10 | 69.9 | 77.0 | 77.0 | 67.3 | 77.0 |
| children=1, memory_mib=256, strategy=cold, vcpus=1 | total | 10 | 1884.2 | 1948.3 | 1948.3 | 1815.7 | 1948.3 |
| children=1, memory_mib=256, strategy=cold, vcpus=1 | children | 10 | 1884.2 | 1948.3 | 1948.3 | 1815.7 | 1948.3 |
| children=1, memory_mib=256, strategy=cold, vcpus=1 | child_p50 | 10 | 1883.7 | 1947.7 | 1947.7 | 1814.3 | 1947.7 |
| children=1, memory_mib=256, strategy=copy, vcpus=1 | total | 10 | 217.1 | 230.1 | 230.1 | 212.9 | 230.1 |
| children=1, memory_mib=256, strategy=copy, vcpus=1 | paused | 10 | 63.5 | 71.8 | 71.8 | 62.8 | 71.8 |
| children=1, memory_mib=256, strategy=copy, vcpus=1 | children | 10 | 150.9 | 161.8 | 161.8 | 146.8 | 161.8 |
| children=1, memory_mib=256, strategy=copy, vcpus=1 | child_p50 | 10 | 150.6 | 161.6 | 161.6 | 146.6 | 161.6 |
| children=1, memory_mib=256, strategy=share, vcpus=1 | total | 10 | 137.5 | 141.7 | 141.7 | 130.6 | 141.7 |
| children=1, memory_mib=256, strategy=share, vcpus=1 | paused | 10 | 63.4 | 64.1 | 64.1 | 63.1 | 64.1 |
| children=1, memory_mib=256, strategy=share, vcpus=1 | children | 10 | 71.5 | 75.7 | 75.7 | 64.7 | 75.7 |
| children=1, memory_mib=256, strategy=share, vcpus=1 | child_p50 | 10 | 71.3 | 75.4 | 75.4 | 64.5 | 75.4 |
| children=1, memory_mib=256, strategy=uffd, vcpus=1 | total | 10 | 176.0 | 207.5 | 207.5 | 164.9 | 207.5 |
| children=1, memory_mib=256, strategy=uffd, vcpus=1 | paused | 10 | 63.7 | 64.5 | 64.5 | 62.9 | 64.5 |
| children=1, memory_mib=256, strategy=uffd, vcpus=1 | children | 10 | 109.9 | 141.2 | 141.2 | 96.8 | 141.2 |
| children=1, memory_mib=256, strategy=uffd, vcpus=1 | child_p50 | 10 | 109.6 | 140.9 | 140.9 | 96.6 | 140.9 |
| children=1, memory_mib=256, strategy=uffd-prefetch, vcpus=1 | total | 10 | 168.7 | 188.9 | 188.9 | 163.1 | 188.9 |
| children=1, memory_mib=256, strategy=uffd-prefetch, vcpus=1 | paused | 10 | 64.0 | 65.2 | 65.2 | 63.1 | 65.2 |
| children=1, memory_mib=256, strategy=uffd-prefetch, vcpus=1 | first | 10 | 101.8 | 121.4 | 121.4 | 95.9 | 121.4 |
| children=1, memory_mib=256, strategy=uffd-prefetch, vcpus=1 | children | 10 | 0.0 | 0.0 | 0.0 | 0.0 | 0.0 |
| children=1, memory_mib=256, strategy=uffd-prefetch, vcpus=1 | child_p50 | 10 | 99.3 | 121.1 | 121.1 | 95.6 | 121.1 |
| children=10, evicted=True, memory_mib=256, read=True, strategy=share, touch_mib=128, vcpus=1 | total | 10 | 465.3 | 508.5 | 508.5 | 436.7 | 508.5 |
| children=10, evicted=True, memory_mib=256, read=True, strategy=share, touch_mib=128, vcpus=1 | paused | 10 | 61.7 | 62.6 | 62.6 | 60.5 | 62.6 |
| children=10, evicted=True, memory_mib=256, read=True, strategy=share, touch_mib=128, vcpus=1 | children | 10 | 397.9 | 440.2 | 440.2 | 368.1 | 440.2 |
| children=10, evicted=True, memory_mib=256, read=True, strategy=share, touch_mib=128, vcpus=1 | child_p50 | 10 | 127.9 | 151.1 | 151.1 | 111.2 | 151.1 |
| children=10, evicted=True, memory_mib=256, read=True, strategy=uffd, touch_mib=128, vcpus=1 | total | 10 | 620.5 | 720.1 | 720.1 | 575.4 | 720.1 |
| children=10, evicted=True, memory_mib=256, read=True, strategy=uffd, touch_mib=128, vcpus=1 | paused | 10 | 61.4 | 62.8 | 62.8 | 60.2 | 62.8 |
| children=10, evicted=True, memory_mib=256, read=True, strategy=uffd, touch_mib=128, vcpus=1 | children | 10 | 551.9 | 651.7 | 651.7 | 507.5 | 651.7 |
| children=10, evicted=True, memory_mib=256, read=True, strategy=uffd, touch_mib=128, vcpus=1 | child_p50 | 10 | 180.4 | 231.8 | 231.8 | 175.8 | 231.8 |
| children=10, features=diff-snapshots, memory_mib=256, strategy=share, touch_mib=128, vcpus=1 | total | 10 | 467.7 | 493.2 | 493.2 | 440.0 | 493.2 |
| children=10, features=diff-snapshots, memory_mib=256, strategy=share, touch_mib=128, vcpus=1 | paused | 10 | 61.8 | 62.9 | 62.9 | 60.7 | 62.9 |
| children=10, features=diff-snapshots, memory_mib=256, strategy=share, touch_mib=128, vcpus=1 | children | 10 | 393.3 | 421.2 | 421.2 | 369.4 | 421.2 |
| children=10, features=diff-snapshots, memory_mib=256, strategy=share, touch_mib=128, vcpus=1 | child_p50 | 10 | 139.8 | 164.5 | 164.5 | 135.3 | 164.5 |
| children=10, features=diff-snapshots, memory_mib=256, strategy=share, vcpus=1 | total | 10 | 458.1 | 502.0 | 502.0 | 437.5 | 502.0 |
| children=10, features=diff-snapshots, memory_mib=256, strategy=share, vcpus=1 | paused | 10 | 64.8 | 65.2 | 65.2 | 64.0 | 65.2 |
| children=10, features=diff-snapshots, memory_mib=256, strategy=share, vcpus=1 | children | 10 | 387.5 | 431.8 | 431.8 | 366.9 | 431.8 |
| children=10, features=diff-snapshots, memory_mib=256, strategy=share, vcpus=1 | child_p50 | 10 | 138.7 | 150.2 | 150.2 | 129.5 | 150.2 |
| children=10, features=huge-pages-2M, memory_mib=256, strategy=uffd, touch_mib=128, vcpus=1 | total | 10 | 347.1 | 352.9 | 352.9 | 331.2 | 352.9 |
| children=10, features=huge-pages-2M, memory_mib=256, strategy=uffd, touch_mib=128, vcpus=1 | paused | 10 | 60.7 | 62.3 | 62.3 | 59.6 | 62.3 |
| children=10, features=huge-pages-2M, memory_mib=256, strategy=uffd, touch_mib=128, vcpus=1 | children | 10 | 283.2 | 289.7 | 289.7 | 267.8 | 289.7 |
| children=10, features=huge-pages-2M, memory_mib=256, strategy=uffd, touch_mib=128, vcpus=1 | child_p50 | 10 | 104.9 | 114.4 | 114.4 | 98.5 | 114.4 |
| children=10, features=huge-pages-2M, memory_mib=256, strategy=uffd, vcpus=1 | total | 10 | 351.8 | 392.1 | 392.1 | 340.0 | 392.1 |
| children=10, features=huge-pages-2M, memory_mib=256, strategy=uffd, vcpus=1 | paused | 10 | 63.9 | 64.3 | 64.3 | 62.9 | 64.3 |
| children=10, features=huge-pages-2M, memory_mib=256, strategy=uffd, vcpus=1 | children | 10 | 285.2 | 324.8 | 324.8 | 274.4 | 324.8 |
| children=10, features=huge-pages-2M, memory_mib=256, strategy=uffd, vcpus=1 | child_p50 | 10 | 103.1 | 121.7 | 121.7 | 96.4 | 121.7 |
| children=10, features=huge-pages-2M, memory_mib=256, strategy=uffd-prefetch, touch_mib=128, vcpus=1 | total | 10 | 341.9 | 355.6 | 355.6 | 334.9 | 355.6 |
| children=10, features=huge-pages-2M, memory_mib=256, strategy=uffd-prefetch, touch_mib=128, vcpus=1 | paused | 10 | 60.9 | 62.5 | 62.5 | 60.2 | 62.5 |
| children=10, features=huge-pages-2M, memory_mib=256, strategy=uffd-prefetch, touch_mib=128, vcpus=1 | first | 10 | 53.1 | 57.5 | 57.5 | 49.3 | 57.5 |
| children=10, features=huge-pages-2M, memory_mib=256, strategy=uffd-prefetch, touch_mib=128, vcpus=1 | children | 10 | 223.9 | 239.9 | 239.9 | 216.5 | 239.9 |
| children=10, features=huge-pages-2M, memory_mib=256, strategy=uffd-prefetch, touch_mib=128, vcpus=1 | child_p50 | 10 | 84.1 | 97.5 | 97.5 | 75.9 | 97.5 |
| children=10, features=huge-pages-2M, memory_mib=256, strategy=uffd-prefetch, vcpus=1 | total | 10 | 342.3 | 354.3 | 354.3 | 338.8 | 354.3 |
| children=10, features=huge-pages-2M, memory_mib=256, strategy=uffd-prefetch, vcpus=1 | paused | 10 | 64.5 | 64.8 | 64.8 | 64.0 | 64.8 |
| children=10, features=huge-pages-2M, memory_mib=256, strategy=uffd-prefetch, vcpus=1 | first | 10 | 52.2 | 53.7 | 53.7 | 48.5 | 53.7 |
| children=10, features=huge-pages-2M, memory_mib=256, strategy=uffd-prefetch, vcpus=1 | children | 10 | 225.5 | 234.8 | 234.8 | 218.4 | 234.8 |
| children=10, features=huge-pages-2M, memory_mib=256, strategy=uffd-prefetch, vcpus=1 | child_p50 | 10 | 92.2 | 98.0 | 98.0 | 85.9 | 98.0 |
| children=10, features=thp, memory_mib=256, strategy=share, touch_mib=128, vcpus=1 | total | 10 | 391.7 | 500.2 | 500.2 | 375.7 | 500.2 |
| children=10, features=thp, memory_mib=256, strategy=share, touch_mib=128, vcpus=1 | paused | 10 | 59.9 | 60.4 | 60.4 | 59.1 | 60.4 |
| children=10, features=thp, memory_mib=256, strategy=share, touch_mib=128, vcpus=1 | children | 10 | 329.2 | 437.5 | 437.5 | 313.1 | 437.5 |
| children=10, features=thp, memory_mib=256, strategy=share, touch_mib=128, vcpus=1 | child_p50 | 10 | 120.1 | 141.9 | 141.9 | 111.2 | 141.9 |
| children=10, features=thp, memory_mib=256, strategy=share, vcpus=1 | total | 10 | 395.3 | 505.1 | 505.1 | 367.4 | 505.1 |
| children=10, features=thp, memory_mib=256, strategy=share, vcpus=1 | paused | 10 | 54.5 | 54.9 | 54.9 | 54.1 | 54.9 |
| children=10, features=thp, memory_mib=256, strategy=share, vcpus=1 | children | 10 | 338.5 | 447.8 | 447.8 | 310.3 | 447.8 |
| children=10, features=thp, memory_mib=256, strategy=share, vcpus=1 | child_p50 | 10 | 120.2 | 150.3 | 150.3 | 111.8 | 150.3 |
| children=10, memory_mib=256, read=True, strategy=share, touch_mib=128, vcpus=1 | total | 10 | 412.5 | 492.4 | 492.4 | 390.1 | 492.4 |
| children=10, memory_mib=256, read=True, strategy=share, touch_mib=128, vcpus=1 | paused | 10 | 61.1 | 61.8 | 61.8 | 60.4 | 61.8 |
| children=10, memory_mib=256, read=True, strategy=share, touch_mib=128, vcpus=1 | children | 10 | 348.9 | 428.8 | 428.8 | 327.2 | 428.8 |
| children=10, memory_mib=256, read=True, strategy=share, touch_mib=128, vcpus=1 | child_p50 | 10 | 127.0 | 143.8 | 143.8 | 115.2 | 143.8 |
| children=10, memory_mib=256, read=True, strategy=uffd, touch_mib=128, vcpus=1 | total | 10 | 560.5 | 625.7 | 625.7 | 538.8 | 625.7 |
| children=10, memory_mib=256, read=True, strategy=uffd, touch_mib=128, vcpus=1 | paused | 10 | 61.3 | 63.8 | 63.8 | 59.7 | 63.8 |
| children=10, memory_mib=256, read=True, strategy=uffd, touch_mib=128, vcpus=1 | children | 10 | 496.5 | 560.6 | 560.6 | 474.3 | 560.6 |
| children=10, memory_mib=256, read=True, strategy=uffd, touch_mib=128, vcpus=1 | child_p50 | 10 | 177.7 | 213.0 | 213.0 | 169.2 | 213.0 |
| children=10, memory_mib=256, strategy=cold, vcpus=1 | total | 10 | 7478.1 | 7569.2 | 7569.2 | 7350.2 | 7569.2 |
| children=10, memory_mib=256, strategy=cold, vcpus=1 | children | 10 | 7478.1 | 7569.2 | 7569.2 | 7350.2 | 7569.2 |
| children=10, memory_mib=256, strategy=cold, vcpus=1 | child_p50 | 10 | 2520.1 | 2604.0 | 2604.0 | 2431.1 | 2604.0 |
| children=10, memory_mib=256, strategy=copy, touch_mib=128, vcpus=1 | total | 10 | 803.4 | 1010.2 | 1010.2 | 788.9 | 1010.2 |
| children=10, memory_mib=256, strategy=copy, touch_mib=128, vcpus=1 | paused | 10 | 61.5 | 62.2 | 62.2 | 61.0 | 62.2 |
| children=10, memory_mib=256, strategy=copy, touch_mib=128, vcpus=1 | children | 10 | 739.6 | 946.3 | 946.3 | 725.4 | 946.3 |
| children=10, memory_mib=256, strategy=copy, touch_mib=128, vcpus=1 | child_p50 | 10 | 276.6 | 360.9 | 360.9 | 264.7 | 360.9 |
| children=10, memory_mib=256, strategy=copy, vcpus=1 | total | 10 | 803.1 | 841.6 | 841.6 | 785.7 | 841.6 |
| children=10, memory_mib=256, strategy=copy, vcpus=1 | paused | 10 | 64.3 | 65.1 | 65.1 | 63.5 | 65.1 |
| children=10, memory_mib=256, strategy=copy, vcpus=1 | children | 10 | 736.8 | 774.8 | 774.8 | 718.1 | 774.8 |
| children=10, memory_mib=256, strategy=copy, vcpus=1 | child_p50 | 10 | 269.9 | 279.0 | 279.0 | 250.2 | 279.0 |
| children=10, memory_mib=256, strategy=share, touch_mib=128, vcpus=1 | total | 10 | 428.8 | 478.6 | 478.6 | 382.2 | 478.6 |
| children=10, memory_mib=256, strategy=share, touch_mib=128, vcpus=1 | paused | 10 | 61.0 | 63.0 | 63.0 | 60.5 | 63.0 |
| children=10, memory_mib=256, strategy=share, touch_mib=128, vcpus=1 | children | 10 | 363.3 | 414.6 | 414.6 | 319.4 | 414.6 |
| children=10, memory_mib=256, strategy=share, touch_mib=128, vcpus=1 | child_p50 | 10 | 123.5 | 150.2 | 150.2 | 111.2 | 150.2 |
| children=10, memory_mib=256, strategy=share, vcpus=1 | total | 10 | 397.9 | 509.9 | 509.9 | 377.4 | 509.9 |
| children=10, memory_mib=256, strategy=share, vcpus=1 | paused | 10 | 64.2 | 64.7 | 64.7 | 63.4 | 64.7 |
| children=10, memory_mib=256, strategy=share, vcpus=1 | children | 10 | 331.8 | 443.2 | 443.2 | 310.9 | 443.2 |
| children=10, memory_mib=256, strategy=share, vcpus=1 | child_p50 | 10 | 122.5 | 149.4 | 149.4 | 108.4 | 149.4 |
| children=10, memory_mib=256, strategy=uffd, touch_mib=128, vcpus=1 | total | 10 | 582.9 | 637.9 | 637.9 | 536.3 | 637.9 |
| children=10, memory_mib=256, strategy=uffd, touch_mib=128, vcpus=1 | paused | 10 | 61.1 | 62.7 | 62.7 | 60.3 | 62.7 |
| children=10, memory_mib=256, strategy=uffd, touch_mib=128, vcpus=1 | children | 10 | 519.0 | 572.7 | 572.7 | 473.7 | 572.7 |
| children=10, memory_mib=256, strategy=uffd, touch_mib=128, vcpus=1 | child_p50 | 10 | 182.6 | 200.4 | 200.4 | 175.1 | 200.4 |
| children=10, memory_mib=256, strategy=uffd, vcpus=1 | total | 10 | 575.5 | 613.7 | 613.7 | 533.5 | 613.7 |
| children=10, memory_mib=256, strategy=uffd, vcpus=1 | paused | 10 | 64.2 | 64.8 | 64.8 | 63.2 | 64.8 |
| children=10, memory_mib=256, strategy=uffd, vcpus=1 | children | 10 | 508.9 | 545.9 | 545.9 | 466.3 | 545.9 |
| children=10, memory_mib=256, strategy=uffd, vcpus=1 | child_p50 | 10 | 183.5 | 210.9 | 210.9 | 174.4 | 210.9 |
| children=10, memory_mib=256, strategy=uffd-prefetch, touch_mib=128, vcpus=1 | total | 10 | 450.6 | 493.8 | 493.8 | 425.9 | 493.8 |
| children=10, memory_mib=256, strategy=uffd-prefetch, touch_mib=128, vcpus=1 | paused | 10 | 60.3 | 63.1 | 63.1 | 59.6 | 63.1 |
| children=10, memory_mib=256, strategy=uffd-prefetch, touch_mib=128, vcpus=1 | first | 10 | 109.4 | 135.1 | 135.1 | 93.8 | 135.1 |
| children=10, memory_mib=256, strategy=uffd-prefetch, touch_mib=128, vcpus=1 | children | 10 | 271.3 | 318.4 | 318.4 | 259.1 | 318.4 |
| children=10, memory_mib=256, strategy=uffd-prefetch, touch_mib=128, vcpus=1 | child_p50 | 10 | 109.2 | 133.6 | 133.6 | 102.3 | 133.6 |
| children=10, memory_mib=256, strategy=uffd-prefetch, vcpus=1 | total | 10 | 459.9 | 518.6 | 518.6 | 448.2 | 518.6 |
| children=10, memory_mib=256, strategy=uffd-prefetch, vcpus=1 | paused | 10 | 64.1 | 65.0 | 65.0 | 63.4 | 65.0 |
| children=10, memory_mib=256, strategy=uffd-prefetch, vcpus=1 | first | 10 | 104.2 | 115.9 | 115.9 | 87.4 | 115.9 |
| children=10, memory_mib=256, strategy=uffd-prefetch, vcpus=1 | children | 10 | 292.9 | 337.1 | 337.1 | 275.2 | 337.1 |
| children=10, memory_mib=256, strategy=uffd-prefetch, vcpus=1 | child_p50 | 10 | 111.7 | 137.1 | 137.1 | 104.3 | 137.1 |
| children=30, features=diff-snapshots, memory_mib=256, strategy=share, vcpus=1 | total | 10 | 1173.9 | 1276.6 | 1276.6 | 1148.8 | 1276.6 |
| children=30, features=diff-snapshots, memory_mib=256, strategy=share, vcpus=1 | paused | 10 | 65.1 | 66.3 | 66.3 | 63.8 | 66.3 |
| children=30, features=diff-snapshots, memory_mib=256, strategy=share, vcpus=1 | children | 10 | 1103.3 | 1205.6 | 1205.6 | 1077.9 | 1205.6 |
| children=30, features=diff-snapshots, memory_mib=256, strategy=share, vcpus=1 | child_p50 | 10 | 142.0 | 153.2 | 153.2 | 140.9 | 153.2 |
| children=30, features=thp, memory_mib=256, strategy=share, vcpus=1 | total | 10 | 1006.3 | 1098.0 | 1098.0 | 990.4 | 1098.0 |
| children=30, features=thp, memory_mib=256, strategy=share, vcpus=1 | paused | 10 | 55.0 | 55.2 | 55.2 | 53.0 | 55.2 |
| children=30, features=thp, memory_mib=256, strategy=share, vcpus=1 | children | 10 | 950.6 | 1040.4 | 1040.4 | 933.0 | 1040.4 |
| children=30, features=thp, memory_mib=256, strategy=share, vcpus=1 | child_p50 | 10 | 120.6 | 128.3 | 128.3 | 118.1 | 128.3 |
| children=30, memory_mib=256, strategy=cold, vcpus=1 | total | 10 | 21271.1 | 21570.1 | 21570.1 | 20847.2 | 21570.1 |
| children=30, memory_mib=256, strategy=cold, vcpus=1 | children | 10 | 21271.1 | 21570.1 | 21570.1 | 20847.2 | 21570.1 |
| children=30, memory_mib=256, strategy=cold, vcpus=1 | child_p50 | 10 | 2732.9 | 2811.5 | 2811.5 | 2684.1 | 2811.5 |
| children=30, memory_mib=256, strategy=copy, vcpus=1 | total | 10 | 10910.5 | 11792.7 | 11792.7 | 10075.1 | 11792.7 |
| children=30, memory_mib=256, strategy=copy, vcpus=1 | paused | 10 | 65.2 | 65.8 | 65.8 | 63.4 | 65.8 |
| children=30, memory_mib=256, strategy=copy, vcpus=1 | children | 10 | 10842.1 | 11725.0 | 11725.0 | 10006.4 | 11725.0 |
| children=30, memory_mib=256, strategy=copy, vcpus=1 | child_p50 | 10 | 310.7 | 368.4 | 368.4 | 290.4 | 368.4 |
| children=30, memory_mib=256, strategy=share, vcpus=1 | total | 10 | 1012.7 | 1095.5 | 1095.5 | 998.4 | 1095.5 |
| children=30, memory_mib=256, strategy=share, vcpus=1 | paused | 10 | 64.6 | 65.4 | 65.4 | 63.9 | 65.4 |
| children=30, memory_mib=256, strategy=share, vcpus=1 | children | 10 | 945.3 | 1027.8 | 1027.8 | 930.7 | 1027.8 |
| children=30, memory_mib=256, strategy=share, vcpus=1 | child_p50 | 10 | 125.0 | 129.3 | 129.3 | 117.0 | 129.3 |
| children=30, memory_mib=256, strategy=uffd, vcpus=1 | total | 10 | 1597.0 | 1651.6 | 1651.6 | 1536.9 | 1651.6 |
| children=30, memory_mib=256, strategy=uffd, vcpus=1 | paused | 10 | 65.1 | 65.9 | 65.9 | 63.2 | 65.9 |
| children=30, memory_mib=256, strategy=uffd, vcpus=1 | children | 10 | 1529.3 | 1583.8 | 1583.8 | 1469.5 | 1583.8 |
| children=30, memory_mib=256, strategy=uffd, vcpus=1 | child_p50 | 10 | 196.2 | 206.1 | 206.1 | 187.1 | 206.1 |
| children=30, memory_mib=256, strategy=uffd-prefetch, vcpus=1 | total | 10 | 1105.0 | 1228.1 | 1228.1 | 1073.3 | 1228.1 |
| children=30, memory_mib=256, strategy=uffd-prefetch, vcpus=1 | paused | 10 | 64.9 | 65.8 | 65.8 | 63.1 | 65.8 |
| children=30, memory_mib=256, strategy=uffd-prefetch, vcpus=1 | first | 10 | 100.4 | 132.4 | 132.4 | 88.3 | 132.4 |
| children=30, memory_mib=256, strategy=uffd-prefetch, vcpus=1 | children | 10 | 928.5 | 1048.9 | 1048.9 | 908.7 | 1048.9 |
| children=30, memory_mib=256, strategy=uffd-prefetch, vcpus=1 | child_p50 | 10 | 123.3 | 138.3 | 138.3 | 119.8 | 138.3 |

| params | child RSS (MiB) | child PSS (MiB) | parent RSS (MiB) |
|---|---|---|---|
| children=1, dirty_mib=0, features=diff-snapshots, memory_mib=256, strategy=share, vcpus=1 | 93 | 93 | 110 |
| children=1, dirty_mib=0, memory_mib=256, strategy=copy, vcpus=1 | 93 | 93 | 110 |
| children=1, dirty_mib=0, memory_mib=256, strategy=share, vcpus=1 | 102 | 102 | 110 |
| children=1, dirty_mib=64, features=diff-snapshots, memory_mib=256, strategy=share, vcpus=1 | 91 | 91 | 169 |
| children=1, dirty_mib=64, memory_mib=256, strategy=copy, vcpus=1 | 94 | 94 | 170 |
| children=1, dirty_mib=64, memory_mib=256, strategy=share, vcpus=1 | 103 | 103 | 170 |
| children=1, features=diff-snapshots, memory_mib=256, strategy=share, vcpus=1 | 106 | 106 | 110 |
| children=1, features=huge-pages-2M, memory_mib=256, strategy=uffd, vcpus=1 | 3 | 3 | 110 |
| children=1, features=huge-pages-2M, memory_mib=256, strategy=uffd-prefetch, vcpus=1 | 3 | 3 | 110 |
| children=1, features=thp, memory_mib=256, strategy=share, vcpus=1 | 108 | 108 | 119 |
| children=1, memory_mib=256, strategy=cold, vcpus=1 | 109 | 109 | nan |
| children=1, memory_mib=256, strategy=copy, vcpus=1 | 96 | 96 | 109 |
| children=1, memory_mib=256, strategy=share, vcpus=1 | 105 | 105 | 110 |
| children=1, memory_mib=256, strategy=uffd, vcpus=1 | 20 | 20 | 110 |
| children=1, memory_mib=256, strategy=uffd-prefetch, vcpus=1 | 20 | 20 | 109 |
| children=10, evicted=True, memory_mib=256, read=True, strategy=share, touch_mib=128, vcpus=1 | 104 | 21 | 234 |
| children=10, evicted=True, memory_mib=256, read=True, strategy=uffd, touch_mib=128, vcpus=1 | 22 | 22 | 234 |
| children=10, features=diff-snapshots, memory_mib=256, strategy=share, touch_mib=128, vcpus=1 | 158 | 73 | 234 |
| children=10, features=diff-snapshots, memory_mib=256, strategy=share, vcpus=1 | 105 | 21 | 110 |
| children=10, features=huge-pages-2M, memory_mib=256, strategy=uffd, touch_mib=128, vcpus=1 | 3 | 3 | 234 |
| children=10, features=huge-pages-2M, memory_mib=256, strategy=uffd, vcpus=1 | 3 | 3 | 110 |
| children=10, features=huge-pages-2M, memory_mib=256, strategy=uffd-prefetch, touch_mib=128, vcpus=1 | 3 | 3 | 234 |
| children=10, features=huge-pages-2M, memory_mib=256, strategy=uffd-prefetch, vcpus=1 | 3 | 3 | 110 |
| children=10, features=thp, memory_mib=256, strategy=share, touch_mib=128, vcpus=1 | 161 | 77 | 244 |
| children=10, features=thp, memory_mib=256, strategy=share, vcpus=1 | 109 | 24 | 119 |
| children=10, memory_mib=256, read=True, strategy=share, touch_mib=128, vcpus=1 | 109 | 22 | 234 |
| children=10, memory_mib=256, read=True, strategy=uffd, touch_mib=128, vcpus=1 | 22 | 22 | 233 |
| children=10, memory_mib=256, strategy=cold, vcpus=1 | 109 | 109 | nan |
| children=10, memory_mib=256, strategy=copy, touch_mib=128, vcpus=1 | 147 | 147 | 233 |
| children=10, memory_mib=256, strategy=copy, vcpus=1 | 97 | 97 | 110 |
| children=10, memory_mib=256, strategy=share, touch_mib=128, vcpus=1 | 156 | 73 | 234 |
| children=10, memory_mib=256, strategy=share, vcpus=1 | 104 | 20 | 110 |
| children=10, memory_mib=256, strategy=uffd, touch_mib=128, vcpus=1 | 73 | 73 | 233 |
| children=10, memory_mib=256, strategy=uffd, vcpus=1 | 20 | 20 | 110 |
| children=10, memory_mib=256, strategy=uffd-prefetch, touch_mib=128, vcpus=1 | 73 | 73 | 234 |
| children=10, memory_mib=256, strategy=uffd-prefetch, vcpus=1 | 20 | 20 | 110 |
| children=30, features=diff-snapshots, memory_mib=256, strategy=share, vcpus=1 | 106 | 14 | 110 |
| children=30, features=thp, memory_mib=256, strategy=share, vcpus=1 | 110 | 18 | 120 |
| children=30, memory_mib=256, strategy=cold, vcpus=1 | 109 | 109 | nan |
| children=30, memory_mib=256, strategy=copy, vcpus=1 | 95 | 95 | 110 |
| children=30, memory_mib=256, strategy=share, vcpus=1 | 105 | 14 | 110 |
| children=30, memory_mib=256, strategy=uffd, vcpus=1 | 20 | 20 | 110 |
| children=30, memory_mib=256, strategy=uffd-prefetch, vcpus=1 | 20 | 20 | 110 |

| params | child write after the fork (ns per 4 KiB page, p50 of children) |
|---|---|
| children=10, evicted=True, memory_mib=256, read=True, strategy=share, touch_mib=128, vcpus=1 | 146 |
| children=10, evicted=True, memory_mib=256, read=True, strategy=uffd, touch_mib=128, vcpus=1 | 205 |
| children=10, features=diff-snapshots, memory_mib=256, strategy=share, touch_mib=128, vcpus=1 | 62453 |
| children=10, features=huge-pages-2M, memory_mib=256, strategy=uffd, touch_mib=128, vcpus=1 | 1114 |
| children=10, features=huge-pages-2M, memory_mib=256, strategy=uffd-prefetch, touch_mib=128, vcpus=1 | 1115 |
| children=10, features=thp, memory_mib=256, strategy=share, touch_mib=128, vcpus=1 | 62465 |
| children=10, memory_mib=256, read=True, strategy=share, touch_mib=128, vcpus=1 | 137 |
| children=10, memory_mib=256, read=True, strategy=uffd, touch_mib=128, vcpus=1 | 190 |
| children=10, memory_mib=256, strategy=copy, touch_mib=128, vcpus=1 | 62548 |
| children=10, memory_mib=256, strategy=share, touch_mib=128, vcpus=1 | 62331 |
| children=10, memory_mib=256, strategy=uffd, touch_mib=128, vcpus=1 | 47631 |
| children=10, memory_mib=256, strategy=uffd-prefetch, touch_mib=128, vcpus=1 | 47366 |

## idle

- `{'features': 'balloon', 'memory_mib': 256, 'strategy': 'cold', 'vms': 20}`: host_used (MiB) 2117.0, ksm_saved (MiB) 0.0, pss_total (MiB) 2183.8, rss_total (MiB) 2184.0
- `{'features': 'balloon', 'memory_mib': 256, 'strategy': 'share', 'vms': 20}`: host_used (MiB) 270.0, ksm_saved (MiB) 0.0, pss_total (MiB) 331.9, rss_total (MiB) 2091.6
- `{'features': 'ksm', 'memory_mib': 256, 'strategy': 'cold', 'vms': 20}`: host_used (MiB) 809.0, ksm_saved (MiB) 1354.9, pss_total (MiB) 830.6, rss_total (MiB) 2184.9
- `{'features': 'ksm', 'memory_mib': 256, 'strategy': 'share', 'vms': 20}`: host_used (MiB) 143.0, ksm_saved (MiB) 146.6, pss_total (MiB) 219.0, rss_total (MiB) 2171.5
- `{'features': 'ksm', 'memory_mib': 256, 'strategy': 'uffd', 'vms': 20}`: host_used (MiB) 165.0, ksm_saved (MiB) 329.8, pss_total (MiB) 130.8, rss_total (MiB) 423.3
- `{'features': 'thp', 'memory_mib': 256, 'strategy': 'cold', 'vms': 20}`: host_used (MiB) 2306.0, ksm_saved (MiB) 0.0, pss_total (MiB) 2378.3, rss_total (MiB) 2378.6
- `{'features': 'thp', 'memory_mib': 256, 'strategy': 'share', 'vms': 20}`: host_used (MiB) 367.0, ksm_saved (MiB) 0.0, pss_total (MiB) 412.0, rss_total (MiB) 2164.7
- `{'memory_mib': 256, 'strategy': 'cold', 'vms': 20}`: host_used (MiB) 2121.0, ksm_saved (MiB) 0.0, pss_total (MiB) 2181.2, rss_total (MiB) 2181.4
- `{'memory_mib': 256, 'strategy': 'share', 'vms': 20}`: host_used (MiB) 265.0, ksm_saved (MiB) 0.0, pss_total (MiB) 336.8, rss_total (MiB) 2167.0
- `{'memory_mib': 256, 'strategy': 'uffd', 'vms': 20}`: host_used (MiB) 477.0, ksm_saved (MiB) 0.0, pss_total (MiB) 422.7, rss_total (MiB) 422.9

## pressure

- `{'children': 24, 'memory_mib': 2048, 'step_mib': 128}`: rounds 5.0

## rollback

| params | measure | n | p50 | p95 | p99 | min | max |
|---|---|---|---|---|---|---|---|
|  | rollback | 100 | 66.3 | 73.9 | 76.6 | 57.7 | 82.1 |

## snapshot

| params | measure | n | p50 | p95 | p99 | min | max |
|---|---|---|---|---|---|---|---|
| features=diff-snapshots, memory_mib=1024 | paused | 10 | 4.0 | 7.5 | 7.5 | 3.8 | 7.5 |
| features=diff-snapshots, memory_mib=1024 | total | 10 | 29.4 | 34.9 | 34.9 | 28.0 | 34.9 |
| features=diff-snapshots, memory_mib=1024 | wait | 10 | 2025.4 | 2042.1 | 2042.1 | 2014.2 | 2042.1 |
| features=diff-snapshots, memory_mib=256 | paused | 10 | 4.2 | 5.3 | 5.3 | 3.5 | 5.3 |
| features=diff-snapshots, memory_mib=256 | total | 10 | 30.0 | 31.6 | 31.6 | 28.8 | 31.6 |
| features=diff-snapshots, memory_mib=256 | wait | 10 | 2030.5 | 2041.2 | 2041.2 | 2026.4 | 2041.2 |
| features=diff-snapshots, memory_mib=4096 | paused | 10 | 6.1 | 6.4 | 6.4 | 6.0 | 6.4 |
| features=diff-snapshots, memory_mib=4096 | total | 10 | 36.6 | 37.8 | 37.8 | 34.7 | 37.8 |
| features=diff-snapshots, memory_mib=4096 | wait | 10 | 2026.6 | 2037.1 | 2037.1 | 2010.7 | 2037.1 |
| features=thp, memory_mib=1024 | paused | 10 | 187.1 | 190.7 | 190.7 | 186.0 | 190.7 |
| features=thp, memory_mib=1024 | total | 10 | 187.8 | 191.5 | 191.5 | 186.7 | 191.5 |
| features=thp, memory_mib=1024 | wait | 10 | 9187.1 | 9214.8 | 9214.8 | 9185.9 | 9214.8 |
| features=thp, memory_mib=256 | paused | 10 | 53.1 | 56.0 | 56.0 | 52.5 | 56.0 |
| features=thp, memory_mib=256 | total | 10 | 53.8 | 56.7 | 56.7 | 53.1 | 56.7 |
| features=thp, memory_mib=256 | wait | 10 | 3048.6 | 3049.2 | 3049.2 | 3047.1 | 3049.2 |
| features=thp, memory_mib=4096 | paused | 10 | 724.2 | 3995.9 | 3995.9 | 711.1 | 3995.9 |
| features=thp, memory_mib=4096 | total | 10 | 725.0 | 3996.7 | 3996.7 | 711.9 | 3996.7 |
| features=thp, memory_mib=4096 | wait | 10 | 33274.7 | 33277.6 | 33277.6 | 30004.2 | 33277.6 |
| memory_mib=1024 | paused | 10 | 157.0 | 159.1 | 159.1 | 156.6 | 159.1 |
| memory_mib=1024 | total | 10 | 157.8 | 159.9 | 159.9 | 157.3 | 159.9 |
| memory_mib=1024 | wait | 10 | 9186.1 | 9219.7 | 9219.7 | 9092.0 | 9219.7 |
| memory_mib=256 | paused | 10 | 47.6 | 52.0 | 52.0 | 47.1 | 52.0 |
| memory_mib=256 | total | 10 | 48.5 | 52.7 | 52.7 | 47.8 | 52.7 |
| memory_mib=256 | wait | 10 | 3048.4 | 3049.2 | 3049.2 | 3047.6 | 3049.2 |
| memory_mib=4096 | paused | 10 | 601.4 | 4215.3 | 4215.3 | 596.6 | 4215.3 |
| memory_mib=4096 | total | 10 | 602.2 | 4216.0 | 4216.0 | 597.4 | 4216.0 |
| memory_mib=4096 | wait | 10 | 33350.9 | 33356.6 | 33356.6 | 29738.0 | 33356.6 |

Left out: create-cold 6, create-template 12, density 3, exec 3, fork 168, idle 30, pressure 3, rollback 3, snapshot 27
