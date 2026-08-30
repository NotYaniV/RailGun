-- Single-path workload. The path comes from the runner so the workload table
-- in Go stays the single source of truth for what is being requested.
local path = os.getenv("RAILGUN_LIGHT_PATH") or "/bytes/1024"

request = function()
  return wrk.format("GET", path)
end
