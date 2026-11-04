package product

import "embed"

// shippedSkills are the skills the product carries in its binary (see Skills).
//
//go:embed all:skills
var shippedSkills embed.FS
