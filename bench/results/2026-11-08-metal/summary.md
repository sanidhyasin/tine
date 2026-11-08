# Results: c5d.metal, bare metal

| Host | |
|---|---|
| Instance | c5d.metal |
| Virtualization | none |
| CPU | Intel(R) Xeon(R) Platinum 8275CL CPU @ 3.00GHz |
| CPUs | 96 |
| Memory (MiB) | 193021 |
| Kernel | 7.0.0-1014-aws |
| Firecracker | Firecracker v1.17.0 |
| tine | v0.3.0-24-g92a035c |
| Data disk | nvme0n1: Amazon EC2 NVMe Instance Storage |
| vm.dirty_ratio | 60 |
| THP | madvise |
| KSM | 0 |

Times are in milliseconds. Warm-up and failed runs are left out; how many is noted.

## create-cold

| params | measure | n | p50 | p95 | p99 | min | max |
|---|---|---|---|---|---|---|---|
| features=thp, memory_mib=1024, vcpus=2 | total | 100 | 760.6 | 779.9 | 791.9 | 732.7 | 811.2 |
| features=thp, memory_mib=1024, vcpus=2 | client | 100 | 761.4 | 780.4 | 792.4 | 733.1 | 811.7 |
| features=thp, memory_mib=1024, vcpus=2 | destroy | 100 | 36.7 | 59.4 | 65.7 | 27.6 | 71.1 |
| memory_mib=1024, vcpus=2 | total | 100 | 802.9 | 857.2 | 887.0 | 772.4 | 888.2 |
| memory_mib=1024, vcpus=2 | client | 100 | 803.4 | 857.9 | 887.6 | 772.9 | 888.8 |
| memory_mib=1024, vcpus=2 | destroy | 100 | 45.4 | 65.0 | 67.5 | 35.6 | 70.1 |

## create-template

| params | measure | n | p50 | p95 | p99 | min | max |
|---|---|---|---|---|---|---|---|
| memory_mib=1024, strategy=copy, vcpus=2 | total | 100 | 401.3 | 413.3 | 449.8 | 388.8 | 467.2 |
| memory_mib=1024, strategy=copy, vcpus=2 | client | 100 | 401.8 | 414.1 | 450.4 | 389.3 | 467.7 |
| memory_mib=1024, strategy=copy, vcpus=2 | destroy | 100 | 35.5 | 46.7 | 49.1 | 28.9 | 49.7 |
| memory_mib=1024, strategy=uffd, vcpus=2 | total | 100 | 68.0 | 79.7 | 87.2 | 60.4 | 90.7 |
| memory_mib=1024, strategy=uffd, vcpus=2 | client | 100 | 68.8 | 80.2 | 87.8 | 61.0 | 91.3 |
| memory_mib=1024, strategy=uffd, vcpus=2 | destroy | 100 | 37.6 | 62.3 | 74.1 | 26.9 | 77.1 |
| memory_mib=1024, strategy=uffd-prefetch, vcpus=2 | total | 100 | 45.8 | 55.1 | 55.9 | 36.4 | 57.3 |
| memory_mib=1024, strategy=uffd-prefetch, vcpus=2 | client | 100 | 46.3 | 55.6 | 57.9 | 36.9 | 73.5 |
| memory_mib=1024, strategy=uffd-prefetch, vcpus=2 | destroy | 100 | 41.5 | 62.7 | 66.4 | 27.4 | 67.6 |
| memory_mib=1024, vcpus=2 | total | 100 | 46.9 | 54.7 | 60.7 | 38.3 | 69.3 |
| memory_mib=1024, vcpus=2 | client | 100 | 47.4 | 57.9 | 67.6 | 38.7 | 69.9 |
| memory_mib=1024, vcpus=2 | destroy | 100 | 34.4 | 47.7 | 56.5 | 26.4 | 56.9 |

## density

- `{'memory_mib': 256, 'vcpus': 1}`: child_pss (MiB) 8.9, child_rss (MiB) 41.3, children 550.0, host_available (MiB) 184229.0, parent_pss (MiB) 108.9, parent_rss (MiB) 108.9, seconds 30.1

## exec

| params | measure | n | p50 | p95 | p99 | min | max |
|---|---|---|---|---|---|---|---|
| command=true | client | 200 | 1.0 | 1.4 | 1.9 | 0.8 | 2.1 |
| command=true | guest | 200 | 0.5 | 0.7 | 0.8 | 0.4 | 1.2 |

## fork

| params | measure | n | p50 | p95 | p99 | min | max |
|---|---|---|---|---|---|---|---|
| children=1, dirty_mib=0, features=diff-snapshots, memory_mib=1024, strategy=share, vcpus=1 | total | 10 | 3225.9 | 3287.7 | 3287.7 | 2612.9 | 3287.7 |
| children=1, dirty_mib=0, features=diff-snapshots, memory_mib=1024, strategy=share, vcpus=1 | paused | 10 | 10.9 | 41.7 | 41.7 | 10.6 | 41.7 |
| children=1, dirty_mib=0, features=diff-snapshots, memory_mib=1024, strategy=share, vcpus=1 | children | 10 | 45.9 | 59.2 | 59.2 | 41.8 | 59.2 |
| children=1, dirty_mib=0, features=diff-snapshots, memory_mib=1024, strategy=share, vcpus=1 | child_p50 | 10 | 45.5 | 58.9 | 58.9 | 41.5 | 58.9 |
| children=1, dirty_mib=0, memory_mib=1024, strategy=copy, vcpus=1 | total | 10 | 3495.7 | 4913.4 | 4913.4 | 3442.3 | 4913.4 |
| children=1, dirty_mib=0, memory_mib=1024, strategy=copy, vcpus=1 | paused | 10 | 250.6 | 1729.0 | 1729.0 | 241.8 | 1729.0 |
| children=1, dirty_mib=0, memory_mib=1024, strategy=copy, vcpus=1 | children | 10 | 414.7 | 458.8 | 458.8 | 394.6 | 458.8 |
| children=1, dirty_mib=0, memory_mib=1024, strategy=copy, vcpus=1 | child_p50 | 10 | 405.8 | 458.3 | 458.3 | 394.3 | 458.3 |
| children=1, dirty_mib=0, memory_mib=1024, strategy=share, vcpus=1 | total | 10 | 3220.7 | 4234.4 | 4234.4 | 3069.1 | 4234.4 |
| children=1, dirty_mib=0, memory_mib=1024, strategy=share, vcpus=1 | paused | 10 | 248.8 | 1412.8 | 1412.8 | 239.5 | 1412.8 |
| children=1, dirty_mib=0, memory_mib=1024, strategy=share, vcpus=1 | children | 10 | 44.4 | 780.8 | 780.8 | 42.1 | 780.8 |
| children=1, dirty_mib=0, memory_mib=1024, strategy=share, vcpus=1 | child_p50 | 10 | 44.2 | 780.5 | 780.5 | 41.8 | 780.5 |
| children=1, dirty_mib=256, features=diff-snapshots, memory_mib=1024, strategy=share, vcpus=1 | total | 10 | 3361.1 | 4623.9 | 4623.9 | 3347.4 | 4623.9 |
| children=1, dirty_mib=256, features=diff-snapshots, memory_mib=1024, strategy=share, vcpus=1 | paused | 10 | 104.2 | 1364.2 | 1364.2 | 101.3 | 1364.2 |
| children=1, dirty_mib=256, features=diff-snapshots, memory_mib=1024, strategy=share, vcpus=1 | children | 10 | 46.7 | 62.1 | 62.1 | 41.2 | 62.1 |
| children=1, dirty_mib=256, features=diff-snapshots, memory_mib=1024, strategy=share, vcpus=1 | child_p50 | 10 | 46.5 | 61.8 | 61.8 | 40.9 | 61.8 |
| children=1, dirty_mib=256, memory_mib=1024, strategy=copy, vcpus=1 | total | 10 | 3497.1 | 4561.1 | 4561.1 | 3437.2 | 4561.1 |
| children=1, dirty_mib=256, memory_mib=1024, strategy=copy, vcpus=1 | paused | 10 | 272.4 | 1338.5 | 1338.5 | 257.4 | 1338.5 |
| children=1, dirty_mib=256, memory_mib=1024, strategy=copy, vcpus=1 | children | 10 | 409.9 | 458.6 | 458.6 | 399.6 | 458.6 |
| children=1, dirty_mib=256, memory_mib=1024, strategy=copy, vcpus=1 | child_p50 | 10 | 409.7 | 458.4 | 458.4 | 399.4 | 458.4 |
| children=1, dirty_mib=256, memory_mib=1024, strategy=share, vcpus=1 | total | 10 | 3102.8 | 4076.4 | 4076.4 | 2977.9 | 4076.4 |
| children=1, dirty_mib=256, memory_mib=1024, strategy=share, vcpus=1 | paused | 10 | 273.4 | 1245.3 | 1245.3 | 269.4 | 1245.3 |
| children=1, dirty_mib=256, memory_mib=1024, strategy=share, vcpus=1 | children | 10 | 49.1 | 769.1 | 769.1 | 39.6 | 769.1 |
| children=1, dirty_mib=256, memory_mib=1024, strategy=share, vcpus=1 | child_p50 | 10 | 48.8 | 768.8 | 768.8 | 39.4 | 768.8 |
| children=1, dirty_mib=64, features=diff-snapshots, memory_mib=1024, strategy=share, vcpus=1 | total | 10 | 3252.2 | 3332.4 | 3332.4 | 2671.6 | 3332.4 |
| children=1, dirty_mib=64, features=diff-snapshots, memory_mib=1024, strategy=share, vcpus=1 | paused | 10 | 39.2 | 73.5 | 73.5 | 38.0 | 73.5 |
| children=1, dirty_mib=64, features=diff-snapshots, memory_mib=1024, strategy=share, vcpus=1 | children | 10 | 45.5 | 54.7 | 54.7 | 39.5 | 54.7 |
| children=1, dirty_mib=64, features=diff-snapshots, memory_mib=1024, strategy=share, vcpus=1 | child_p50 | 10 | 45.2 | 54.4 | 54.4 | 39.3 | 54.4 |
| children=1, dirty_mib=64, memory_mib=1024, strategy=copy, vcpus=1 | total | 10 | 3427.1 | 4836.8 | 4836.8 | 3409.8 | 4836.8 |
| children=1, dirty_mib=64, memory_mib=1024, strategy=copy, vcpus=1 | paused | 10 | 253.6 | 1654.4 | 1654.4 | 246.2 | 1654.4 |
| children=1, dirty_mib=64, memory_mib=1024, strategy=copy, vcpus=1 | children | 10 | 400.4 | 418.1 | 418.1 | 390.2 | 418.1 |
| children=1, dirty_mib=64, memory_mib=1024, strategy=copy, vcpus=1 | child_p50 | 10 | 400.2 | 417.9 | 417.9 | 390.0 | 417.9 |
| children=1, dirty_mib=64, memory_mib=1024, strategy=share, vcpus=1 | total | 10 | 3746.7 | 4406.6 | 4406.6 | 3069.9 | 4406.6 |
| children=1, dirty_mib=64, memory_mib=1024, strategy=share, vcpus=1 | paused | 10 | 250.9 | 1582.2 | 1582.2 | 244.9 | 1582.2 |
| children=1, dirty_mib=64, memory_mib=1024, strategy=share, vcpus=1 | children | 10 | 53.2 | 795.0 | 795.0 | 42.7 | 795.0 |
| children=1, dirty_mib=64, memory_mib=1024, strategy=share, vcpus=1 | child_p50 | 10 | 52.9 | 794.7 | 794.7 | 42.5 | 794.7 |
| children=1, features=diff-snapshots, memory_mib=256, strategy=share, vcpus=1 | total | 10 | 168.2 | 211.1 | 211.1 | 158.9 | 211.1 |
| children=1, features=diff-snapshots, memory_mib=256, strategy=share, vcpus=1 | paused | 10 | 108.6 | 149.4 | 149.4 | 104.9 | 149.4 |
| children=1, features=diff-snapshots, memory_mib=256, strategy=share, vcpus=1 | children | 10 | 54.4 | 59.1 | 59.1 | 40.9 | 59.1 |
| children=1, features=diff-snapshots, memory_mib=256, strategy=share, vcpus=1 | child_p50 | 10 | 54.1 | 58.9 | 58.9 | 40.7 | 58.9 |
| children=1, features=huge-pages-2M, memory_mib=256, strategy=uffd, vcpus=1 | total | 10 | 192.8 | 234.3 | 234.3 | 185.3 | 234.3 |
| children=1, features=huge-pages-2M, memory_mib=256, strategy=uffd, vcpus=1 | paused | 10 | 108.8 | 154.0 | 154.0 | 107.0 | 154.0 |
| children=1, features=huge-pages-2M, memory_mib=256, strategy=uffd, vcpus=1 | children | 10 | 74.2 | 84.6 | 84.6 | 68.6 | 84.6 |
| children=1, features=huge-pages-2M, memory_mib=256, strategy=uffd, vcpus=1 | child_p50 | 10 | 73.9 | 84.2 | 84.2 | 68.3 | 84.2 |
| children=1, features=huge-pages-2M, memory_mib=256, strategy=uffd-prefetch, vcpus=1 | total | 10 | 186.2 | 223.5 | 223.5 | 162.3 | 223.5 |
| children=1, features=huge-pages-2M, memory_mib=256, strategy=uffd-prefetch, vcpus=1 | paused | 10 | 106.2 | 150.8 | 150.8 | 93.4 | 150.8 |
| children=1, features=huge-pages-2M, memory_mib=256, strategy=uffd-prefetch, vcpus=1 | first | 10 | 66.0 | 80.5 | 80.5 | 62.3 | 80.5 |
| children=1, features=huge-pages-2M, memory_mib=256, strategy=uffd-prefetch, vcpus=1 | children | 10 | 0.0 | 0.0 | 0.0 | 0.0 | 0.0 |
| children=1, features=huge-pages-2M, memory_mib=256, strategy=uffd-prefetch, vcpus=1 | child_p50 | 10 | 65.7 | 80.3 | 80.3 | 62.0 | 80.3 |
| children=1, features=thp, memory_mib=256, strategy=share, vcpus=1 | total | 10 | 153.3 | 168.0 | 168.0 | 144.8 | 168.0 |
| children=1, features=thp, memory_mib=256, strategy=share, vcpus=1 | paused | 10 | 89.9 | 94.1 | 94.1 | 87.8 | 94.1 |
| children=1, features=thp, memory_mib=256, strategy=share, vcpus=1 | children | 10 | 59.1 | 66.0 | 66.0 | 47.6 | 66.0 |
| children=1, features=thp, memory_mib=256, strategy=share, vcpus=1 | child_p50 | 10 | 58.9 | 65.8 | 65.8 | 47.3 | 65.8 |
| children=1, memory_mib=256, strategy=cold, vcpus=1 | total | 10 | 918.8 | 931.8 | 931.8 | 890.1 | 931.8 |
| children=1, memory_mib=256, strategy=cold, vcpus=1 | children | 10 | 918.8 | 931.8 | 931.8 | 890.1 | 931.8 |
| children=1, memory_mib=256, strategy=cold, vcpus=1 | child_p50 | 10 | 918.3 | 931.2 | 931.2 | 889.6 | 931.2 |
| children=1, memory_mib=256, strategy=copy, vcpus=1 | total | 10 | 256.1 | 370.5 | 370.5 | 236.3 | 370.5 |
| children=1, memory_mib=256, strategy=copy, vcpus=1 | paused | 10 | 106.7 | 223.0 | 223.0 | 92.6 | 223.0 |
| children=1, memory_mib=256, strategy=copy, vcpus=1 | children | 10 | 143.2 | 160.2 | 160.2 | 137.4 | 160.2 |
| children=1, memory_mib=256, strategy=copy, vcpus=1 | child_p50 | 10 | 143.0 | 160.0 | 160.0 | 137.1 | 160.0 |
| children=1, memory_mib=256, strategy=share, vcpus=1 | total | 10 | 167.5 | 213.3 | 213.3 | 145.5 | 213.3 |
| children=1, memory_mib=256, strategy=share, vcpus=1 | paused | 10 | 106.1 | 145.8 | 145.8 | 91.6 | 145.8 |
| children=1, memory_mib=256, strategy=share, vcpus=1 | children | 10 | 56.2 | 63.2 | 63.2 | 47.3 | 63.2 |
| children=1, memory_mib=256, strategy=share, vcpus=1 | child_p50 | 10 | 55.9 | 62.9 | 62.9 | 46.8 | 62.9 |
| children=1, memory_mib=256, strategy=uffd, vcpus=1 | total | 10 | 190.6 | 222.3 | 222.3 | 163.4 | 222.3 |
| children=1, memory_mib=256, strategy=uffd, vcpus=1 | paused | 10 | 107.3 | 115.2 | 115.2 | 93.1 | 115.2 |
| children=1, memory_mib=256, strategy=uffd, vcpus=1 | children | 10 | 77.1 | 108.6 | 108.6 | 65.7 | 108.6 |
| children=1, memory_mib=256, strategy=uffd, vcpus=1 | child_p50 | 10 | 76.9 | 108.3 | 108.3 | 65.4 | 108.3 |
| children=1, memory_mib=256, strategy=uffd-prefetch, vcpus=1 | total | 10 | 197.9 | 261.2 | 261.2 | 180.3 | 261.2 |
| children=1, memory_mib=256, strategy=uffd-prefetch, vcpus=1 | paused | 10 | 110.4 | 151.1 | 151.1 | 93.3 | 151.1 |
| children=1, memory_mib=256, strategy=uffd-prefetch, vcpus=1 | first | 10 | 84.0 | 104.2 | 104.2 | 78.2 | 104.2 |
| children=1, memory_mib=256, strategy=uffd-prefetch, vcpus=1 | children | 10 | 0.0 | 0.0 | 0.0 | 0.0 | 0.0 |
| children=1, memory_mib=256, strategy=uffd-prefetch, vcpus=1 | child_p50 | 10 | 82.2 | 103.9 | 103.9 | 73.4 | 103.9 |
| children=10, evicted=True, memory_mib=256, read=True, strategy=share, touch_mib=128, vcpus=1 | total | 10 | 310.1 | 415.7 | 415.7 | 247.1 | 415.7 |
| children=10, evicted=True, memory_mib=256, read=True, strategy=share, touch_mib=128, vcpus=1 | paused | 10 | 87.0 | 89.8 | 89.8 | 85.9 | 89.8 |
| children=10, evicted=True, memory_mib=256, read=True, strategy=share, touch_mib=128, vcpus=1 | children | 10 | 209.9 | 320.0 | 320.0 | 151.3 | 320.0 |
| children=10, evicted=True, memory_mib=256, read=True, strategy=share, touch_mib=128, vcpus=1 | child_p50 | 10 | 137.6 | 155.8 | 155.8 | 89.7 | 155.8 |
| children=10, evicted=True, memory_mib=256, read=True, strategy=uffd, touch_mib=128, vcpus=1 | total | 10 | 317.4 | 358.3 | 358.3 | 283.7 | 358.3 |
| children=10, evicted=True, memory_mib=256, read=True, strategy=uffd, touch_mib=128, vcpus=1 | paused | 10 | 91.9 | 151.9 | 151.9 | 87.5 | 151.9 |
| children=10, evicted=True, memory_mib=256, read=True, strategy=uffd, touch_mib=128, vcpus=1 | children | 10 | 198.7 | 257.6 | 257.6 | 178.2 | 257.6 |
| children=10, evicted=True, memory_mib=256, read=True, strategy=uffd, touch_mib=128, vcpus=1 | child_p50 | 10 | 149.3 | 183.4 | 183.4 | 123.4 | 183.4 |
| children=10, features=diff-snapshots, memory_mib=256, strategy=share, touch_mib=128, vcpus=1 | total | 10 | 212.6 | 279.1 | 279.1 | 194.1 | 279.1 |
| children=10, features=diff-snapshots, memory_mib=256, strategy=share, touch_mib=128, vcpus=1 | paused | 10 | 88.2 | 156.7 | 156.7 | 85.4 | 156.7 |
| children=10, features=diff-snapshots, memory_mib=256, strategy=share, touch_mib=128, vcpus=1 | children | 10 | 113.4 | 160.3 | 160.3 | 100.2 | 160.3 |
| children=10, features=diff-snapshots, memory_mib=256, strategy=share, touch_mib=128, vcpus=1 | child_p50 | 10 | 104.0 | 112.0 | 112.0 | 92.2 | 112.0 |
| children=10, features=diff-snapshots, memory_mib=256, strategy=share, vcpus=1 | total | 10 | 228.0 | 250.3 | 250.3 | 187.8 | 250.3 |
| children=10, features=diff-snapshots, memory_mib=256, strategy=share, vcpus=1 | paused | 10 | 107.3 | 123.3 | 123.3 | 103.2 | 123.3 |
| children=10, features=diff-snapshots, memory_mib=256, strategy=share, vcpus=1 | children | 10 | 111.2 | 135.9 | 135.9 | 79.4 | 135.9 |
| children=10, features=diff-snapshots, memory_mib=256, strategy=share, vcpus=1 | child_p50 | 10 | 100.1 | 110.9 | 110.9 | 74.1 | 110.9 |
| children=10, features=huge-pages-2M, memory_mib=256, strategy=uffd, touch_mib=128, vcpus=1 | total | 10 | 237.7 | 266.4 | 266.4 | 228.5 | 266.4 |
| children=10, features=huge-pages-2M, memory_mib=256, strategy=uffd, touch_mib=128, vcpus=1 | paused | 10 | 87.9 | 95.7 | 95.7 | 85.1 | 95.7 |
| children=10, features=huge-pages-2M, memory_mib=256, strategy=uffd, touch_mib=128, vcpus=1 | children | 10 | 146.1 | 166.0 | 166.0 | 135.8 | 166.0 |
| children=10, features=huge-pages-2M, memory_mib=256, strategy=uffd, touch_mib=128, vcpus=1 | child_p50 | 10 | 135.3 | 150.5 | 150.5 | 120.3 | 150.5 |
| children=10, features=huge-pages-2M, memory_mib=256, strategy=uffd, vcpus=1 | total | 10 | 259.2 | 268.9 | 268.9 | 246.7 | 268.9 |
| children=10, features=huge-pages-2M, memory_mib=256, strategy=uffd, vcpus=1 | paused | 10 | 107.0 | 111.7 | 111.7 | 105.2 | 111.7 |
| children=10, features=huge-pages-2M, memory_mib=256, strategy=uffd, vcpus=1 | children | 10 | 148.9 | 154.8 | 154.8 | 133.7 | 154.8 |
| children=10, features=huge-pages-2M, memory_mib=256, strategy=uffd, vcpus=1 | child_p50 | 10 | 134.3 | 144.2 | 144.2 | 125.4 | 144.2 |
| children=10, features=huge-pages-2M, memory_mib=256, strategy=uffd-prefetch, touch_mib=128, vcpus=1 | total | 10 | 346.1 | 383.7 | 383.7 | 292.6 | 383.7 |
| children=10, features=huge-pages-2M, memory_mib=256, strategy=uffd-prefetch, touch_mib=128, vcpus=1 | paused | 10 | 91.2 | 110.9 | 110.9 | 86.7 | 110.9 |
| children=10, features=huge-pages-2M, memory_mib=256, strategy=uffd-prefetch, touch_mib=128, vcpus=1 | first | 10 | 70.8 | 84.5 | 84.5 | 57.4 | 84.5 |
| children=10, features=huge-pages-2M, memory_mib=256, strategy=uffd-prefetch, touch_mib=128, vcpus=1 | children | 10 | 179.2 | 204.8 | 204.8 | 139.0 | 204.8 |
| children=10, features=huge-pages-2M, memory_mib=256, strategy=uffd-prefetch, touch_mib=128, vcpus=1 | child_p50 | 10 | 130.9 | 137.2 | 137.2 | 125.0 | 137.2 |
| children=10, features=huge-pages-2M, memory_mib=256, strategy=uffd-prefetch, vcpus=1 | total | 10 | 361.1 | 388.1 | 388.1 | 325.6 | 388.1 |
| children=10, features=huge-pages-2M, memory_mib=256, strategy=uffd-prefetch, vcpus=1 | paused | 10 | 110.5 | 128.8 | 128.8 | 104.0 | 128.8 |
| children=10, features=huge-pages-2M, memory_mib=256, strategy=uffd-prefetch, vcpus=1 | first | 10 | 70.1 | 81.6 | 81.6 | 59.4 | 81.6 |
| children=10, features=huge-pages-2M, memory_mib=256, strategy=uffd-prefetch, vcpus=1 | children | 10 | 160.9 | 199.5 | 199.5 | 137.0 | 199.5 |
| children=10, features=huge-pages-2M, memory_mib=256, strategy=uffd-prefetch, vcpus=1 | child_p50 | 10 | 132.9 | 157.2 | 157.2 | 120.5 | 157.2 |
| children=10, features=thp, memory_mib=256, strategy=share, touch_mib=128, vcpus=1 | total | 10 | 208.4 | 270.9 | 270.9 | 193.6 | 270.9 |
| children=10, features=thp, memory_mib=256, strategy=share, touch_mib=128, vcpus=1 | paused | 10 | 79.5 | 144.7 | 144.7 | 74.9 | 144.7 |
| children=10, features=thp, memory_mib=256, strategy=share, touch_mib=128, vcpus=1 | children | 10 | 117.5 | 145.4 | 145.4 | 112.1 | 145.4 |
| children=10, features=thp, memory_mib=256, strategy=share, touch_mib=128, vcpus=1 | child_p50 | 10 | 104.7 | 121.4 | 121.4 | 94.4 | 121.4 |
| children=10, features=thp, memory_mib=256, strategy=share, vcpus=1 | total | 10 | 215.9 | 264.8 | 264.8 | 195.0 | 264.8 |
| children=10, features=thp, memory_mib=256, strategy=share, vcpus=1 | paused | 10 | 90.7 | 132.7 | 132.7 | 73.5 | 132.7 |
| children=10, features=thp, memory_mib=256, strategy=share, vcpus=1 | children | 10 | 118.1 | 168.4 | 168.4 | 105.7 | 168.4 |
| children=10, features=thp, memory_mib=256, strategy=share, vcpus=1 | child_p50 | 10 | 109.2 | 115.3 | 115.3 | 96.9 | 115.3 |
| children=10, memory_mib=256, read=True, strategy=share, touch_mib=128, vcpus=1 | total | 10 | 210.0 | 362.4 | 362.4 | 193.3 | 362.4 |
| children=10, memory_mib=256, read=True, strategy=share, touch_mib=128, vcpus=1 | paused | 10 | 86.7 | 245.7 | 245.7 | 85.0 | 245.7 |
| children=10, memory_mib=256, read=True, strategy=share, touch_mib=128, vcpus=1 | children | 10 | 117.3 | 142.4 | 142.4 | 100.9 | 142.4 |
| children=10, memory_mib=256, read=True, strategy=share, touch_mib=128, vcpus=1 | child_p50 | 10 | 102.0 | 116.3 | 116.3 | 91.1 | 116.3 |
| children=10, memory_mib=256, read=True, strategy=uffd, touch_mib=128, vcpus=1 | total | 10 | 241.9 | 323.9 | 323.9 | 226.2 | 323.9 |
| children=10, memory_mib=256, read=True, strategy=uffd, touch_mib=128, vcpus=1 | paused | 10 | 87.7 | 158.4 | 158.4 | 86.0 | 158.4 |
| children=10, memory_mib=256, read=True, strategy=uffd, touch_mib=128, vcpus=1 | children | 10 | 149.6 | 180.3 | 180.3 | 133.3 | 180.3 |
| children=10, memory_mib=256, read=True, strategy=uffd, touch_mib=128, vcpus=1 | child_p50 | 10 | 138.7 | 148.3 | 148.3 | 117.6 | 148.3 |
| children=10, memory_mib=256, strategy=cold, vcpus=1 | total | 10 | 1079.2 | 1112.3 | 1112.3 | 1071.7 | 1112.3 |
| children=10, memory_mib=256, strategy=cold, vcpus=1 | children | 10 | 1079.2 | 1112.3 | 1112.3 | 1071.7 | 1112.3 |
| children=10, memory_mib=256, strategy=cold, vcpus=1 | child_p50 | 10 | 1059.7 | 1082.3 | 1082.3 | 1050.0 | 1082.3 |
| children=10, memory_mib=256, strategy=copy, touch_mib=128, vcpus=1 | total | 10 | 329.0 | 417.0 | 417.0 | 298.1 | 417.0 |
| children=10, memory_mib=256, strategy=copy, touch_mib=128, vcpus=1 | paused | 10 | 92.1 | 157.2 | 157.2 | 86.2 | 157.2 |
| children=10, memory_mib=256, strategy=copy, touch_mib=128, vcpus=1 | children | 10 | 230.0 | 292.2 | 292.2 | 207.9 | 292.2 |
| children=10, memory_mib=256, strategy=copy, touch_mib=128, vcpus=1 | child_p50 | 10 | 215.2 | 222.5 | 222.5 | 203.0 | 222.5 |
| children=10, memory_mib=256, strategy=copy, vcpus=1 | total | 10 | 365.1 | 394.3 | 394.3 | 320.0 | 394.3 |
| children=10, memory_mib=256, strategy=copy, vcpus=1 | paused | 10 | 106.7 | 135.9 | 135.9 | 101.7 | 135.9 |
| children=10, memory_mib=256, strategy=copy, vcpus=1 | children | 10 | 252.6 | 279.0 | 279.0 | 208.7 | 279.0 |
| children=10, memory_mib=256, strategy=copy, vcpus=1 | child_p50 | 10 | 199.0 | 214.5 | 214.5 | 175.0 | 214.5 |
| children=10, memory_mib=256, strategy=share, touch_mib=128, vcpus=1 | total | 10 | 206.4 | 302.0 | 302.0 | 192.5 | 302.0 |
| children=10, memory_mib=256, strategy=share, touch_mib=128, vcpus=1 | paused | 10 | 86.9 | 165.6 | 165.6 | 85.7 | 165.6 |
| children=10, memory_mib=256, strategy=share, touch_mib=128, vcpus=1 | children | 10 | 114.6 | 135.5 | 135.5 | 101.9 | 135.5 |
| children=10, memory_mib=256, strategy=share, touch_mib=128, vcpus=1 | child_p50 | 10 | 100.1 | 109.9 | 109.9 | 82.2 | 109.9 |
| children=10, memory_mib=256, strategy=share, vcpus=1 | total | 10 | 226.2 | 260.9 | 260.9 | 203.9 | 260.9 |
| children=10, memory_mib=256, strategy=share, vcpus=1 | paused | 10 | 106.1 | 112.4 | 112.4 | 94.6 | 112.4 |
| children=10, memory_mib=256, strategy=share, vcpus=1 | children | 10 | 117.7 | 149.5 | 149.5 | 102.6 | 149.5 |
| children=10, memory_mib=256, strategy=share, vcpus=1 | child_p50 | 10 | 100.4 | 103.5 | 103.5 | 94.2 | 103.5 |
| children=10, memory_mib=256, strategy=uffd, touch_mib=128, vcpus=1 | total | 10 | 247.9 | 284.7 | 284.7 | 241.1 | 284.7 |
| children=10, memory_mib=256, strategy=uffd, touch_mib=128, vcpus=1 | paused | 10 | 89.0 | 151.1 | 151.1 | 87.2 | 151.1 |
| children=10, memory_mib=256, strategy=uffd, touch_mib=128, vcpus=1 | children | 10 | 152.0 | 182.6 | 182.6 | 126.9 | 182.6 |
| children=10, memory_mib=256, strategy=uffd, touch_mib=128, vcpus=1 | child_p50 | 10 | 137.5 | 150.7 | 150.7 | 119.3 | 150.7 |
| children=10, memory_mib=256, strategy=uffd, vcpus=1 | total | 10 | 259.4 | 283.5 | 283.5 | 242.9 | 283.5 |
| children=10, memory_mib=256, strategy=uffd, vcpus=1 | paused | 10 | 108.1 | 122.9 | 122.9 | 95.0 | 122.9 |
| children=10, memory_mib=256, strategy=uffd, vcpus=1 | children | 10 | 145.0 | 168.1 | 168.1 | 126.5 | 168.1 |
| children=10, memory_mib=256, strategy=uffd, vcpus=1 | child_p50 | 10 | 133.7 | 153.4 | 153.4 | 121.6 | 153.4 |
| children=10, memory_mib=256, strategy=uffd-prefetch, touch_mib=128, vcpus=1 | total | 10 | 292.1 | 334.4 | 334.4 | 275.6 | 334.4 |
| children=10, memory_mib=256, strategy=uffd-prefetch, touch_mib=128, vcpus=1 | paused | 10 | 87.6 | 95.0 | 95.0 | 84.1 | 95.0 |
| children=10, memory_mib=256, strategy=uffd-prefetch, touch_mib=128, vcpus=1 | first | 10 | 84.2 | 113.3 | 113.3 | 76.1 | 113.3 |
| children=10, memory_mib=256, strategy=uffd-prefetch, touch_mib=128, vcpus=1 | children | 10 | 111.1 | 155.2 | 155.2 | 101.7 | 155.2 |
| children=10, memory_mib=256, strategy=uffd-prefetch, touch_mib=128, vcpus=1 | child_p50 | 10 | 99.7 | 146.5 | 146.5 | 94.2 | 146.5 |
| children=10, memory_mib=256, strategy=uffd-prefetch, vcpus=1 | total | 10 | 304.6 | 314.5 | 314.5 | 287.3 | 314.5 |
| children=10, memory_mib=256, strategy=uffd-prefetch, vcpus=1 | paused | 10 | 107.0 | 111.0 | 111.0 | 92.1 | 111.0 |
| children=10, memory_mib=256, strategy=uffd-prefetch, vcpus=1 | first | 10 | 81.3 | 92.6 | 92.6 | 67.8 | 92.6 |
| children=10, memory_mib=256, strategy=uffd-prefetch, vcpus=1 | children | 10 | 108.3 | 137.9 | 137.9 | 102.6 | 137.9 |
| children=10, memory_mib=256, strategy=uffd-prefetch, vcpus=1 | child_p50 | 10 | 100.7 | 130.0 | 130.0 | 94.7 | 130.0 |
| children=100, features=diff-snapshots, memory_mib=256, strategy=share, vcpus=1 | total | 10 | 1025.0 | 1070.8 | 1070.8 | 998.2 | 1070.8 |
| children=100, features=diff-snapshots, memory_mib=256, strategy=share, vcpus=1 | paused | 10 | 115.2 | 155.9 | 155.9 | 103.5 | 155.9 |
| children=100, features=diff-snapshots, memory_mib=256, strategy=share, vcpus=1 | children | 10 | 899.6 | 950.7 | 950.7 | 842.1 | 950.7 |
| children=100, features=diff-snapshots, memory_mib=256, strategy=share, vcpus=1 | child_p50 | 10 | 780.9 | 819.4 | 819.4 | 730.9 | 819.4 |
| children=100, features=thp, memory_mib=256, strategy=share, vcpus=1 | total | 10 | 1065.3 | 1259.4 | 1259.4 | 954.8 | 1259.4 |
| children=100, features=thp, memory_mib=256, strategy=share, vcpus=1 | paused | 10 | 97.6 | 147.0 | 147.0 | 78.2 | 147.0 |
| children=100, features=thp, memory_mib=256, strategy=share, vcpus=1 | children | 10 | 919.1 | 1171.3 | 1171.3 | 851.4 | 1171.3 |
| children=100, features=thp, memory_mib=256, strategy=share, vcpus=1 | child_p50 | 10 | 786.2 | 852.5 | 852.5 | 723.7 | 852.5 |
| children=100, memory_mib=256, strategy=cold, vcpus=1 | total | 10 | 4016.7 | 4194.5 | 4194.5 | 3960.3 | 4194.5 |
| children=100, memory_mib=256, strategy=cold, vcpus=1 | children | 10 | 4016.7 | 4194.5 | 4194.5 | 3960.3 | 4194.5 |
| children=100, memory_mib=256, strategy=cold, vcpus=1 | child_p50 | 10 | 2899.5 | 2972.6 | 2972.6 | 2841.1 | 2972.6 |
| children=100, memory_mib=256, strategy=copy, vcpus=1 | total | 10 | 1642.0 | 1755.8 | 1755.8 | 1566.9 | 1755.8 |
| children=100, memory_mib=256, strategy=copy, vcpus=1 | paused | 10 | 104.2 | 155.1 | 155.1 | 96.3 | 155.1 |
| children=100, memory_mib=256, strategy=copy, vcpus=1 | children | 10 | 1526.5 | 1641.1 | 1641.1 | 1466.3 | 1641.1 |
| children=100, memory_mib=256, strategy=copy, vcpus=1 | child_p50 | 10 | 1389.9 | 1538.5 | 1538.5 | 1334.8 | 1538.5 |
| children=100, memory_mib=256, strategy=share, vcpus=1 | total | 10 | 998.1 | 1353.6 | 1353.6 | 946.6 | 1353.6 |
| children=100, memory_mib=256, strategy=share, vcpus=1 | paused | 10 | 152.8 | 233.4 | 233.4 | 101.4 | 233.4 |
| children=100, memory_mib=256, strategy=share, vcpus=1 | children | 10 | 845.4 | 1113.2 | 1113.2 | 820.9 | 1113.2 |
| children=100, memory_mib=256, strategy=share, vcpus=1 | child_p50 | 10 | 746.8 | 822.2 | 822.2 | 736.8 | 822.2 |
| children=100, memory_mib=256, strategy=uffd, vcpus=1 | total | 10 | 1085.7 | 1200.4 | 1200.4 | 1055.8 | 1200.4 |
| children=100, memory_mib=256, strategy=uffd, vcpus=1 | paused | 10 | 115.3 | 157.7 | 157.7 | 100.6 | 157.7 |
| children=100, memory_mib=256, strategy=uffd, vcpus=1 | children | 10 | 956.8 | 1044.9 | 1044.9 | 934.5 | 1044.9 |
| children=100, memory_mib=256, strategy=uffd, vcpus=1 | child_p50 | 10 | 848.5 | 916.4 | 916.4 | 804.5 | 916.4 |
| children=100, memory_mib=256, strategy=uffd-prefetch, vcpus=1 | total | 10 | 1146.1 | 1393.0 | 1393.0 | 1101.4 | 1393.0 |
| children=100, memory_mib=256, strategy=uffd-prefetch, vcpus=1 | paused | 10 | 156.5 | 166.0 | 166.0 | 103.9 | 166.0 |
| children=100, memory_mib=256, strategy=uffd-prefetch, vcpus=1 | first | 10 | 78.4 | 100.9 | 100.9 | 66.4 | 100.9 |
| children=100, memory_mib=256, strategy=uffd-prefetch, vcpus=1 | children | 10 | 935.4 | 1125.0 | 1125.0 | 861.5 | 1125.0 |
| children=100, memory_mib=256, strategy=uffd-prefetch, vcpus=1 | child_p50 | 10 | 782.9 | 834.1 | 834.1 | 730.7 | 834.1 |
| children=30, features=diff-snapshots, memory_mib=256, strategy=share, vcpus=1 | total | 10 | 386.0 | 551.2 | 551.2 | 363.4 | 551.2 |
| children=30, features=diff-snapshots, memory_mib=256, strategy=share, vcpus=1 | paused | 10 | 111.4 | 159.8 | 159.8 | 99.8 | 159.8 |
| children=30, features=diff-snapshots, memory_mib=256, strategy=share, vcpus=1 | children | 10 | 267.0 | 446.5 | 446.5 | 242.9 | 446.5 |
| children=30, features=diff-snapshots, memory_mib=256, strategy=share, vcpus=1 | child_p50 | 10 | 207.8 | 283.9 | 283.9 | 192.2 | 283.9 |
| children=30, features=thp, memory_mib=256, strategy=share, vcpus=1 | total | 10 | 402.3 | 511.5 | 511.5 | 343.1 | 511.5 |
| children=30, features=thp, memory_mib=256, strategy=share, vcpus=1 | paused | 10 | 92.0 | 97.1 | 97.1 | 80.3 | 97.1 |
| children=30, features=thp, memory_mib=256, strategy=share, vcpus=1 | children | 10 | 303.1 | 406.9 | 406.9 | 256.4 | 406.9 |
| children=30, features=thp, memory_mib=256, strategy=share, vcpus=1 | child_p50 | 10 | 215.4 | 236.9 | 236.9 | 192.4 | 236.9 |
| children=30, memory_mib=256, strategy=cold, vcpus=1 | total | 10 | 1514.4 | 1608.5 | 1608.5 | 1484.6 | 1608.5 |
| children=30, memory_mib=256, strategy=cold, vcpus=1 | children | 10 | 1514.4 | 1608.5 | 1608.5 | 1484.6 | 1608.5 |
| children=30, memory_mib=256, strategy=cold, vcpus=1 | child_p50 | 10 | 1419.2 | 1479.0 | 1479.0 | 1381.1 | 1479.0 |
| children=30, memory_mib=256, strategy=copy, vcpus=1 | total | 10 | 559.3 | 641.7 | 641.7 | 511.1 | 641.7 |
| children=30, memory_mib=256, strategy=copy, vcpus=1 | paused | 10 | 107.5 | 151.2 | 151.2 | 91.8 | 151.2 |
| children=30, memory_mib=256, strategy=copy, vcpus=1 | children | 10 | 447.1 | 495.9 | 495.9 | 406.1 | 495.9 |
| children=30, memory_mib=256, strategy=copy, vcpus=1 | child_p50 | 10 | 389.7 | 451.0 | 451.0 | 359.3 | 451.0 |
| children=30, memory_mib=256, strategy=share, vcpus=1 | total | 10 | 373.8 | 478.4 | 478.4 | 329.9 | 478.4 |
| children=30, memory_mib=256, strategy=share, vcpus=1 | paused | 10 | 108.4 | 228.6 | 228.6 | 98.3 | 228.6 |
| children=30, memory_mib=256, strategy=share, vcpus=1 | children | 10 | 258.2 | 304.6 | 304.6 | 218.1 | 304.6 |
| children=30, memory_mib=256, strategy=share, vcpus=1 | child_p50 | 10 | 201.4 | 213.1 | 213.1 | 168.1 | 213.1 |
| children=30, memory_mib=256, strategy=uffd, vcpus=1 | total | 10 | 441.8 | 542.3 | 542.3 | 410.5 | 542.3 |
| children=30, memory_mib=256, strategy=uffd, vcpus=1 | paused | 10 | 110.8 | 231.7 | 231.7 | 101.2 | 231.7 |
| children=30, memory_mib=256, strategy=uffd, vcpus=1 | children | 10 | 309.7 | 333.4 | 333.4 | 290.7 | 333.4 |
| children=30, memory_mib=256, strategy=uffd, vcpus=1 | child_p50 | 10 | 266.5 | 295.6 | 295.6 | 241.3 | 295.6 |
| children=30, memory_mib=256, strategy=uffd-prefetch, vcpus=1 | total | 10 | 584.2 | 664.7 | 664.7 | 493.1 | 664.7 |
| children=30, memory_mib=256, strategy=uffd-prefetch, vcpus=1 | paused | 10 | 117.1 | 252.4 | 252.4 | 97.9 | 252.4 |
| children=30, memory_mib=256, strategy=uffd-prefetch, vcpus=1 | first | 10 | 83.4 | 95.9 | 95.9 | 71.4 | 95.9 |
| children=30, memory_mib=256, strategy=uffd-prefetch, vcpus=1 | children | 10 | 331.1 | 401.3 | 401.3 | 294.6 | 401.3 |
| children=30, memory_mib=256, strategy=uffd-prefetch, vcpus=1 | child_p50 | 10 | 270.8 | 319.2 | 319.2 | 247.9 | 319.2 |

| params | child RSS (MiB) | child PSS (MiB) | parent RSS (MiB) |
|---|---|---|---|
| children=1, dirty_mib=0, features=diff-snapshots, memory_mib=1024, strategy=share, vcpus=1 | 101 | 101 | 127 |
| children=1, dirty_mib=0, memory_mib=1024, strategy=copy, vcpus=1 | 102 | 102 | 126 |
| children=1, dirty_mib=0, memory_mib=1024, strategy=share, vcpus=1 | 108 | 108 | 127 |
| children=1, dirty_mib=256, features=diff-snapshots, memory_mib=1024, strategy=share, vcpus=1 | 103 | 103 | 378 |
| children=1, dirty_mib=256, memory_mib=1024, strategy=copy, vcpus=1 | 103 | 103 | 378 |
| children=1, dirty_mib=256, memory_mib=1024, strategy=share, vcpus=1 | 109 | 109 | 378 |
| children=1, dirty_mib=64, features=diff-snapshots, memory_mib=1024, strategy=share, vcpus=1 | 102 | 102 | 186 |
| children=1, dirty_mib=64, memory_mib=1024, strategy=copy, vcpus=1 | 102 | 102 | 186 |
| children=1, dirty_mib=64, memory_mib=1024, strategy=share, vcpus=1 | 109 | 109 | 186 |
| children=1, features=diff-snapshots, memory_mib=256, strategy=share, vcpus=1 | 107 | 107 | 110 |
| children=1, features=huge-pages-2M, memory_mib=256, strategy=uffd, vcpus=1 | 3 | 3 | 111 |
| children=1, features=huge-pages-2M, memory_mib=256, strategy=uffd-prefetch, vcpus=1 | 3 | 3 | 110 |
| children=1, features=thp, memory_mib=256, strategy=share, vcpus=1 | 110 | 110 | 119 |
| children=1, memory_mib=256, strategy=cold, vcpus=1 | 109 | 109 | nan |
| children=1, memory_mib=256, strategy=copy, vcpus=1 | 96 | 96 | 110 |
| children=1, memory_mib=256, strategy=share, vcpus=1 | 107 | 107 | 110 |
| children=1, memory_mib=256, strategy=uffd, vcpus=1 | 20 | 20 | 110 |
| children=1, memory_mib=256, strategy=uffd-prefetch, vcpus=1 | 20 | 20 | 110 |
| children=10, evicted=True, memory_mib=256, read=True, strategy=share, touch_mib=128, vcpus=1 | 106 | 21 | 234 |
| children=10, evicted=True, memory_mib=256, read=True, strategy=uffd, touch_mib=128, vcpus=1 | 21 | 21 | 234 |
| children=10, features=diff-snapshots, memory_mib=256, strategy=share, touch_mib=128, vcpus=1 | 159 | 73 | 234 |
| children=10, features=diff-snapshots, memory_mib=256, strategy=share, vcpus=1 | 106 | 21 | 110 |
| children=10, features=huge-pages-2M, memory_mib=256, strategy=uffd, touch_mib=128, vcpus=1 | 3 | 3 | 234 |
| children=10, features=huge-pages-2M, memory_mib=256, strategy=uffd, vcpus=1 | 3 | 3 | 110 |
| children=10, features=huge-pages-2M, memory_mib=256, strategy=uffd-prefetch, touch_mib=128, vcpus=1 | 3 | 3 | 234 |
| children=10, features=huge-pages-2M, memory_mib=256, strategy=uffd-prefetch, vcpus=1 | 3 | 3 | 111 |
| children=10, features=thp, memory_mib=256, strategy=share, touch_mib=128, vcpus=1 | 163 | 77 | 244 |
| children=10, features=thp, memory_mib=256, strategy=share, vcpus=1 | 110 | 24 | 120 |
| children=10, memory_mib=256, read=True, strategy=share, touch_mib=128, vcpus=1 | 111 | 22 | 234 |
| children=10, memory_mib=256, read=True, strategy=uffd, touch_mib=128, vcpus=1 | 21 | 21 | 234 |
| children=10, memory_mib=256, strategy=cold, vcpus=1 | 109 | 109 | nan |
| children=10, memory_mib=256, strategy=copy, touch_mib=128, vcpus=1 | 148 | 148 | 234 |
| children=10, memory_mib=256, strategy=copy, vcpus=1 | 96 | 96 | 111 |
| children=10, memory_mib=256, strategy=share, touch_mib=128, vcpus=1 | 158 | 73 | 234 |
| children=10, memory_mib=256, strategy=share, vcpus=1 | 106 | 20 | 110 |
| children=10, memory_mib=256, strategy=uffd, touch_mib=128, vcpus=1 | 73 | 73 | 235 |
| children=10, memory_mib=256, strategy=uffd, vcpus=1 | 20 | 20 | 110 |
| children=10, memory_mib=256, strategy=uffd-prefetch, touch_mib=128, vcpus=1 | 73 | 73 | 234 |
| children=10, memory_mib=256, strategy=uffd-prefetch, vcpus=1 | 20 | 20 | 110 |
| children=100, features=diff-snapshots, memory_mib=256, strategy=share, vcpus=1 | 105 | 12 | 110 |
| children=100, features=thp, memory_mib=256, strategy=share, vcpus=1 | 110 | 16 | 120 |
| children=100, memory_mib=256, strategy=cold, vcpus=1 | 109 | 109 | nan |
| children=100, memory_mib=256, strategy=copy, vcpus=1 | 95 | 95 | 110 |
| children=100, memory_mib=256, strategy=share, vcpus=1 | 105 | 12 | 110 |
| children=100, memory_mib=256, strategy=uffd, vcpus=1 | 20 | 20 | 110 |
| children=100, memory_mib=256, strategy=uffd-prefetch, vcpus=1 | 20 | 20 | 110 |
| children=30, features=diff-snapshots, memory_mib=256, strategy=share, vcpus=1 | 106 | 14 | 110 |
| children=30, features=thp, memory_mib=256, strategy=share, vcpus=1 | 110 | 18 | 119 |
| children=30, memory_mib=256, strategy=cold, vcpus=1 | 109 | 109 | nan |
| children=30, memory_mib=256, strategy=copy, vcpus=1 | 95 | 95 | 110 |
| children=30, memory_mib=256, strategy=share, vcpus=1 | 105 | 14 | 110 |
| children=30, memory_mib=256, strategy=uffd, vcpus=1 | 20 | 20 | 110 |
| children=30, memory_mib=256, strategy=uffd-prefetch, vcpus=1 | 20 | 20 | 110 |

| params | child write after the fork (ns per 4 KiB page, p50 of children) |
|---|---|
| children=10, evicted=True, memory_mib=256, read=True, strategy=share, touch_mib=128, vcpus=1 | 83 |
| children=10, evicted=True, memory_mib=256, read=True, strategy=uffd, touch_mib=128, vcpus=1 | 116 |
| children=10, features=diff-snapshots, memory_mib=256, strategy=share, touch_mib=128, vcpus=1 | 7678 |
| children=10, features=huge-pages-2M, memory_mib=256, strategy=uffd, touch_mib=128, vcpus=1 | 1615 |
| children=10, features=huge-pages-2M, memory_mib=256, strategy=uffd-prefetch, touch_mib=128, vcpus=1 | 1399 |
| children=10, features=thp, memory_mib=256, strategy=share, touch_mib=128, vcpus=1 | 7678 |
| children=10, memory_mib=256, read=True, strategy=share, touch_mib=128, vcpus=1 | 110 |
| children=10, memory_mib=256, read=True, strategy=uffd, touch_mib=128, vcpus=1 | 118 |
| children=10, memory_mib=256, strategy=copy, touch_mib=128, vcpus=1 | 7862 |
| children=10, memory_mib=256, strategy=share, touch_mib=128, vcpus=1 | 7657 |
| children=10, memory_mib=256, strategy=uffd, touch_mib=128, vcpus=1 | 20980 |
| children=10, memory_mib=256, strategy=uffd-prefetch, touch_mib=128, vcpus=1 | 20052 |

## idle

- `{'features': 'balloon', 'memory_mib': 256, 'strategy': 'cold', 'vms': 20}`: host_used (MiB) 2089.0, ksm_saved (MiB) 0.0, pss_total (MiB) 2180.3, rss_total (MiB) 2180.5
- `{'features': 'balloon', 'memory_mib': 256, 'strategy': 'share', 'vms': 20}`: host_used (MiB) 394.0, ksm_saved (MiB) 0.0, pss_total (MiB) 336.0, rss_total (MiB) 2126.7
- `{'features': 'ksm', 'memory_mib': 256, 'strategy': 'cold', 'vms': 20}`: host_used (MiB) 848.0, ksm_saved (MiB) 1359.9, pss_total (MiB) 814.8, rss_total (MiB) 2173.6
- `{'features': 'ksm', 'memory_mib': 256, 'strategy': 'share', 'vms': 20}`: host_used (MiB) 231.0, ksm_saved (MiB) 155.9, pss_total (MiB) 198.6, rss_total (MiB) 2089.6
- `{'features': 'ksm', 'memory_mib': 256, 'strategy': 'uffd', 'vms': 20}`: host_used (MiB) 249.0, ksm_saved (MiB) 336.0, pss_total (MiB) 129.6, rss_total (MiB) 424.1
- `{'features': 'thp', 'memory_mib': 256, 'strategy': 'cold', 'vms': 20}`: host_used (MiB) 2281.0, ksm_saved (MiB) 0.0, pss_total (MiB) 2387.6, rss_total (MiB) 2387.8
- `{'features': 'thp', 'memory_mib': 256, 'strategy': 'share', 'vms': 20}`: host_used (MiB) 446.0, ksm_saved (MiB) 0.0, pss_total (MiB) 407.5, rss_total (MiB) 2164.3
- `{'memory_mib': 256, 'strategy': 'cold', 'vms': 20}`: host_used (MiB) 2124.0, ksm_saved (MiB) 0.0, pss_total (MiB) 2173.4, rss_total (MiB) 2173.6
- `{'memory_mib': 256, 'strategy': 'share', 'vms': 20}`: host_used (MiB) 303.0, ksm_saved (MiB) 0.0, pss_total (MiB) 334.9, rss_total (MiB) 2167.5
- `{'memory_mib': 256, 'strategy': 'uffd', 'vms': 20}`: host_used (MiB) 460.0, ksm_saved (MiB) 0.0, pss_total (MiB) 422.6, rss_total (MiB) 422.8

## pressure

- `{'children': 96, 'memory_mib': 2560, 'step_mib': 128}`: rounds 15.0

## rollback

| params | measure | n | p50 | p95 | p99 | min | max |
|---|---|---|---|---|---|---|---|
|  | rollback | 100 | 48.3 | 57.8 | 59.3 | 38.8 | 60.5 |

## snapshot

| params | measure | n | p50 | p95 | p99 | min | max |
|---|---|---|---|---|---|---|---|
| features=diff-snapshots, memory_mib=1024 | paused | 10 | 5.2 | 5.9 | 5.9 | 4.9 | 5.9 |
| features=diff-snapshots, memory_mib=1024 | total | 10 | 38.0 | 42.0 | 42.0 | 36.5 | 42.0 |
| features=diff-snapshots, memory_mib=1024 | wait | 10 | 2014.8 | 2020.4 | 2020.4 | 2001.1 | 2020.4 |
| features=diff-snapshots, memory_mib=256 | paused | 10 | 4.5 | 5.4 | 5.4 | 4.3 | 5.4 |
| features=diff-snapshots, memory_mib=256 | total | 10 | 40.6 | 43.0 | 43.0 | 38.1 | 43.0 |
| features=diff-snapshots, memory_mib=256 | wait | 10 | 2018.7 | 2026.6 | 2026.6 | 2001.9 | 2026.6 |
| features=diff-snapshots, memory_mib=4096 | paused | 10 | 9.0 | 11.0 | 11.0 | 8.5 | 11.0 |
| features=diff-snapshots, memory_mib=4096 | total | 10 | 60.7 | 63.8 | 63.8 | 57.9 | 63.8 |
| features=diff-snapshots, memory_mib=4096 | wait | 10 | 2009.1 | 2020.0 | 2020.0 | 2001.1 | 2020.0 |
| features=thp, memory_mib=1024 | paused | 10 | 465.9 | 480.5 | 480.5 | 279.4 | 480.5 |
| features=thp, memory_mib=1024 | total | 10 | 467.4 | 481.2 | 481.2 | 280.0 | 481.2 |
| features=thp, memory_mib=1024 | wait | 10 | 2523.7 | 2790.9 | 2790.9 | 2499.6 | 2790.9 |
| features=thp, memory_mib=256 | paused | 10 | 90.6 | 155.5 | 155.5 | 90.2 | 155.5 |
| features=thp, memory_mib=256 | total | 10 | 91.2 | 156.1 | 156.1 | 90.7 | 156.1 |
| features=thp, memory_mib=256 | wait | 10 | 2147.7 | 2200.2 | 2200.2 | 2112.9 | 2200.2 |
| features=thp, memory_mib=4096 | paused | 10 | 1034.5 | 1929.5 | 1929.5 | 1006.0 | 1929.5 |
| features=thp, memory_mib=4096 | total | 10 | 1035.2 | 1930.6 | 1930.6 | 1006.6 | 1930.6 |
| features=thp, memory_mib=4096 | wait | 10 | 6457.7 | 6458.6 | 6458.6 | 5251.2 | 6458.6 |
| memory_mib=1024 | paused | 10 | 244.8 | 861.9 | 861.9 | 244.0 | 861.9 |
| memory_mib=1024 | total | 10 | 245.4 | 862.5 | 862.5 | 244.6 | 862.5 |
| memory_mib=1024 | wait | 10 | 2773.3 | 2789.2 | 2789.2 | 2112.9 | 2789.2 |
| memory_mib=256 | paused | 10 | 82.8 | 404.0 | 404.0 | 82.3 | 404.0 |
| memory_mib=256 | total | 10 | 83.4 | 404.7 | 404.7 | 82.9 | 404.7 |
| memory_mib=256 | wait | 10 | 2189.1 | 2198.4 | 2198.4 | 2013.4 | 2198.4 |
| memory_mib=4096 | paused | 10 | 899.5 | 914.0 | 914.0 | 882.6 | 914.0 |
| memory_mib=4096 | total | 10 | 900.1 | 915.4 | 915.4 | 883.3 | 915.4 |
| memory_mib=4096 | wait | 10 | 6458.1 | 6459.0 | 6459.0 | 6027.2 | 6459.0 |

Left out: create-cold 6, create-template 12, density 3, exec 3, fork 159, idle 30, pressure 3, rollback 3, snapshot 27
