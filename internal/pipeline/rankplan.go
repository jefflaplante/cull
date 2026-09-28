package pipeline

// maxRankFrames is the largest number of frames sent to the model in a single
// ranking call.
const maxRankFrames = 8

// ceilDiv returns ceil(a/b) for positive b.
func ceilDiv(a, b int) int {
	if b <= 0 {
		return 0
	}
	return (a + b - 1) / b
}

// chunks splits positions 0..n-1 (capture order) into ceil(n/maxRankFrames)
// nearly equal, contiguous chunks, each no larger than maxRankFrames.
func chunks(n int) [][]int {
	if n <= 0 {
		return nil
	}
	k := ceilDiv(n, maxRankFrames)
	result := make([][]int, 0, k)
	for i := 0; i < k; i++ {
		start := i * n / k
		end := (i + 1) * n / k
		chunk := make([]int, 0, end-start)
		for p := start; p < end; p++ {
			chunk = append(chunk, p)
		}
		result = append(result, chunk)
	}
	return result
}

// finalists returns how many top frames from each of nChunks chunks advance
// to the final ranking round, so the final round is at most maxRankFrames
// frames and covers at least keepBest frames overall.
func finalists(nChunks, keepBest int) int {
	if nChunks <= 0 {
		return 1
	}
	f := maxRankFrames / nChunks
	if c := ceilDiv(keepBest, nChunks); c > f {
		f = c
	}
	if f < 1 {
		f = 1
	}
	return f
}

// merge returns the merged frame order: finalOrder (the ranked finalists)
// first, then the non-finalists ordered by their rank within their chunk,
// with ties between chunks broken by position (earlier chunk first, since
// chunkOrders is given in position order).
func merge(chunkOrders [][]int, finalOrder []int) []int {
	isFinalist := make(map[int]bool, len(finalOrder))
	for _, p := range finalOrder {
		isFinalist[p] = true
	}

	maxLen := 0
	for _, order := range chunkOrders {
		if len(order) > maxLen {
			maxLen = len(order)
		}
	}

	result := make([]int, 0, len(finalOrder)+maxLen*len(chunkOrders))
	result = append(result, finalOrder...)
	for r := 0; r < maxLen; r++ {
		for _, order := range chunkOrders {
			if r >= len(order) {
				continue
			}
			p := order[r]
			if !isFinalist[p] {
				result = append(result, p)
			}
		}
	}
	return result
}
