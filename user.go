package discordgo

import (
	"fmt"
	"strings"
)

type PremiumType int

const (
	NotPremium PremiumType = iota
	NitroClassic
	Nitro
)

// A User stores all data for an individual Discord user.
type User struct {
	// The ID of the user.
	ID int64 `json:"id,string"`

	// The user's username.
	Username string `json:"username"`

	// The hash of the user's avatar. Use Session.UserAvatar
	// to retrieve the avatar itself.
	Avatar string `json:"avatar"`

	// The user's chosen language option.
	Locale string `json:"locale"`

	// The discriminator of the user (4 numbers after name).
	Discriminator string `json:"discriminator"`

	// Whether the user is a bot.
	Bot bool `json:"bot"`

	// An user's Nitro status
	PremiumType PremiumType `json:"premium_type"`
}

// String returns a unique identifier of the form username#discriminator
func (u *User) String() string {
	return fmt.Sprintf("%s#%s", u.Username, u.Discriminator)
}

// Mention return a string which mentions the user
func (u *User) Mention() string {
	return fmt.Sprintf("<@%d>", u.ID)
}

// AvatarURL returns a URL to the user's avatar.
//    size:    The size of the user's avatar as a power of two
//             if size is an empty string, no size parameter will
//             be added to the URL.
func (u *User) AvatarURL(size string) string {
	var URL string
	if u.Avatar == "" {
		URL = EndpointDefaultUserAvatar(u.Discriminator)
	} else if strings.HasPrefix(u.Avatar, "a_") {
		URL = EndpointUserAvatarAnimated(u.ID, u.Avatar)
	} else {
		URL = EndpointUserAvatar(u.ID, u.Avatar)
	}

	if size != "" {
		return URL + "?size=" + size
	}
	return URL
}

// A SelfUser stores user data about the token owner.
// Includes a few extra fields than a normal user struct.
type SelfUser struct {
	*User
	Token string `json:"token"`
}
