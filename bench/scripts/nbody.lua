-- Class (b): Benchmark Game N-Body (object field reads/writes + math.sqrt)
local sqrt = math.sqrt
local PI = 3.141592653589793
local SOLAR_MASS = 4 * PI * PI
local DAYS_PER_YEAR = 365.24

local bodies = {
  { x = 0, y = 0, z = 0, vx = 0, vy = 0, vz = 0, mass = SOLAR_MASS },
  {
    x = 4.84143144246472090e+00, y = -1.16032004402742839e+00, z = -1.03622044471123109e-01,
    vx = 1.66007664274403694e-03 * DAYS_PER_YEAR, vy = 7.69901118419740425e-03 * DAYS_PER_YEAR,
    vz = -6.90460016972063023e-05 * DAYS_PER_YEAR, mass = 9.54791938424326609e-04 * SOLAR_MASS,
  },
  {
    x = 8.34336671824457987e+00, y = 4.12479856412430479e+00, z = -4.03523417114321381e-01,
    vx = -2.76742510726862411e-03 * DAYS_PER_YEAR, vy = 4.99852801234917238e-03 * DAYS_PER_YEAR,
    vz = 2.30417297573763929e-05 * DAYS_PER_YEAR, mass = 2.85885980666130812e-04 * SOLAR_MASS,
  },
  {
    x = 1.28943695621391310e+01, y = -1.51111514016986312e+01, z = -2.23307578892655734e-01,
    vx = 2.96460137564761618e-03 * DAYS_PER_YEAR, vy = 2.37847173959480950e-03 * DAYS_PER_YEAR,
    vz = -2.96589568540237556e-05 * DAYS_PER_YEAR, mass = 4.36624404335156298e-05 * SOLAR_MASS,
  },
  {
    x = 1.53796971148509165e+01, y = -2.59193146099879641e+01, z = 1.79258772950371181e-01,
    vx = 2.68067772490389322e-03 * DAYS_PER_YEAR, vy = 1.62824170038242295e-03 * DAYS_PER_YEAR,
    vz = -9.51592254519715870e-05 * DAYS_PER_YEAR, mass = 5.15138902046611451e-05 * SOLAR_MASS,
  },
}

local function offsetMomentum()
  local px, py, pz = 0, 0, 0
  for i = 1, #bodies do
    local b = bodies[i]
    local m = b.mass
    px = px + b.vx * m
    py = py + b.vy * m
    pz = pz + b.vz * m
  end
  local sun = bodies[1]
  sun.vx = -px / SOLAR_MASS
  sun.vy = -py / SOLAR_MASS
  sun.vz = -pz / SOLAR_MASS
end

local function energy()
  local e = 0.0
  local n = #bodies
  for i = 1, n do
    local bi = bodies[i]
    local vx, vy, vz, m = bi.vx, bi.vy, bi.vz, bi.mass
    e = e + 0.5 * m * (vx * vx + vy * vy + vz * vz)
    for j = i + 1, n do
      local bj = bodies[j]
      local dx, dy, dz = bi.x - bj.x, bi.y - bj.y, bi.z - bj.z
      local dist = sqrt(dx * dx + dy * dy + dz * dz)
      e = e - (m * bj.mass) / dist
    end
  end
  return e
end

local function advance(dt)
  local n = #bodies
  for i = 1, n do
    local bi = bodies[i]
    local bix, biy, biz = bi.x, bi.y, bi.z
    local bivx, bivy, bivz, bim = bi.vx, bi.vy, bi.vz, bi.mass
    for j = i + 1, n do
      local bj = bodies[j]
      local dx, dy, dz = bix - bj.x, biy - bj.y, biz - bj.z
      local d2 = dx * dx + dy * dy + dz * dz
      local mag = dt / (d2 * sqrt(d2))
      local bjm = bj.mass * mag
      bivx = bivx - dx * bjm
      bivy = bivy - dy * bjm
      bivz = bivz - dz * bjm
      local bimm = bim * mag
      bj.vx = bj.vx + dx * bimm
      bj.vy = bj.vy + dy * bimm
      bj.vz = bj.vz + dz * bimm
    end
    bi.vx, bi.vy, bi.vz = bivx, bivy, bivz
    bi.x = bix + dt * bivx
    bi.y = biy + dt * bivy
    bi.z = biz + dt * bivz
  end
end

offsetMomentum()
local e0 = energy()
for _ = 1, 200000 do
  advance(0.01)
end
local e1 = energy()
print("nbody", string.format("%.9f %.9f", e0, e1))
