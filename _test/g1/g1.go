package g1

func Map[TC any, TR any](collection []TC, iteratee func(item TC, index int) TR) []TR {
	result := make([]TR, len(collection))

	for i := range collection {
		result[i] = iteratee(collection[i], i)
	}

	return result
}
