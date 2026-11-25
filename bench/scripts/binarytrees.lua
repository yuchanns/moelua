-- Class (b): Benchmark Game Binary-Trees (allocation + GC + record table fields)
local function bottomUpTree(depth)
  if depth > 0 then
    local d = depth - 1
    return { left = bottomUpTree(d), right = bottomUpTree(d) }
  else
    return { left = nil, right = nil }
  end
end

local function itemCheck(tree)
  if tree.left then
    return 1 + itemCheck(tree.left) + itemCheck(tree.right)
  else
    return 1
  end
end

local maxDepth = 13
local stretchDepth = maxDepth + 1
local total = itemCheck(bottomUpTree(stretchDepth))
local longLived = bottomUpTree(maxDepth)
for depth = 4, maxDepth, 2 do
  local iterations = 1 << (maxDepth - depth + 4)
  local check = 0
  for _ = 1, iterations do
    check = check + itemCheck(bottomUpTree(depth))
  end
  total = total + check
end
total = total + itemCheck(longLived)
print("binarytrees", total)
