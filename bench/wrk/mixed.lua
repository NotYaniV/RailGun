-- CPU-skewed workload (W2, spec §4.4).
--
-- Heaviness is assigned per THREAD, not per request. wrk2 owns connections at
-- thread granularity, so a thread marked heavy makes every one of its
-- connections consistently expensive. Randomising per request instead would
-- average the cost evenly across all connections and destroy the per-connection
-- skew that H1 (spec §5.1.1) is entirely about.

local heavy_path   = os.getenv("RAILGUN_HEAVY_PATH")    or "/bytes/1048576"
local light_path   = os.getenv("RAILGUN_LIGHT_PATH")    or "/bytes/1024"
local heavy_target = tonumber(os.getenv("RAILGUN_HEAVY_THREADS")) or 1

local assigned = 0

function setup(thread)
  if assigned < heavy_target then
    thread:set("path", heavy_path)
    assigned = assigned + 1
  else
    thread:set("path", light_path)
  end
end

function init(args)
  req = wrk.format("GET", path)
end

function request()
  return req
end
