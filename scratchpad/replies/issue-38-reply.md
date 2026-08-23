## Where this stands: I could not reproduce the measurement, and I found a real bug next to it

Two separate things below. The honest headline first: **I could not reproduce your numbers**, so I am not claiming this is fixed.

### 1. I measured your exact test on both versions, and both are flat

Built a real 3-node cluster with `replica_count: 1` to match your placement, and ran warp `get` at `c=2` across your size sweep. Then I ran the identical harness against **4.4.48**, the build you last measured, because a "fixed in X" claim is only worth what measuring both versions proves.

| Object size | Your 4.4.48 | My 4.4.48 | My 4.4.57 |
|---|---|---|---|
| 1 MiB | 15 ms | 1 ms | 1 ms |
| 8 MiB | 32 ms | 1 ms | 1 ms |
| 32 MiB | 124 ms | 1 ms | 1 ms |
| 64 MiB | 272 ms | 1 ms | 1 ms |

Flat, not linear, on the build you call broken. I also checked single node with compression on and with erasure on, both flat.

So the buffering is **not** in the path my setup exercises. Something about your deployment is required to trigger it, and I would rather ask than guess, because the last time I built a tidy theory for one of your reports it was wrong and the real cause was somewhere else.

### 2. A real instance of this bug, found while looking, now fixed

The healthy erasure read path has streamed since 4.4.38. But if a data shard is **missing**, it fell back to reading every shard in full, decoding the whole object, and only then emitting byte one. That is exactly the shape you describe, and it was still there.

Fixed in 4.4.58: parity recovery now runs one aligned stripe at a time and reads only as many shards as the code needs.

| | before | after |
|---|---|---|
| storage read before byte 1, 8 MiB object | whole object | 4.19 MB |
| storage read before byte 1, 32 MiB object | whole object | 4.19 MB |
| TTFB, 64 MiB degraded read | 17 to 23 ms | 3 to 6 ms |
| full-read throughput | baseline | unchanged |

The middle rows are the point: first-byte cost is now **constant**, within one byte across a 4x size difference, instead of growing with the object. Peak memory for a degraded read drops from the whole object to one stripe.

### What would settle it

This only triggers when a shard is actually gone, so it may not be your cause at all. Two questions would tell us:

1. Is `erasure.enabled` true on your cluster, and if so what are `data_shards` / `parity_shards`?
2. Are any objects sitting **degraded**? A missing shard makes every read of that object take the old buffering path. The healer logs report reconstructions, and `.ec/<object>/` should contain `data + parity` shard files.

Also worth confirming, because it caught us out on #49: is `compression`, `encryption` or `packing` enabled? On #49 the reporter did not know encryption was on, and it turned out to be the whole cause. A default-config benchmark tells us nothing about an optional wrapper.

If erasure is off and nothing is degraded, then this fix is real but is not your bug, and I would like to keep digging with a trace from your side rather than close it.

4.4.58 has the fix if you want to re-measure either way.
