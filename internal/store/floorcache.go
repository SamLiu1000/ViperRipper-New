package store

import "fmt"

// 楼层选择器缓存：抓到的楼层（骨架、图片、文本名）持久化到 SQLite，
// 重启后即使离线也能打开楼层列表。"清除缓存"按钮删行后可重新抓取。

type FloorRow struct {
	ForumPostID int64
	FloorNo     int
	Text        string // 楼层正文文字（图册名）
	SourceTitle string // vr.php title 回退（"主题名 RE"）
	ImagesJSON  string // []{main,thumb} 的 JSON
}

type FloorMeta struct {
	NextURL string
	Done    bool
}

func (s *Store) LoadFloorCache(rowID int64) ([]FloorRow, FloorMeta, error) {
	rows, err := s.db.Query(`SELECT post_id,no,name,xml_title,images FROM floor_cache WHERE row_id=? ORDER BY no,post_id`, rowID)
	if err != nil {
		return nil, FloorMeta{}, err
	}
	defer rows.Close()
	var out []FloorRow
	for rows.Next() {
		var r FloorRow
		if err := rows.Scan(&r.ForumPostID, &r.FloorNo, &r.Text, &r.SourceTitle, &r.ImagesJSON); err != nil {
			return nil, FloorMeta{}, err
		}
		out = append(out, r)
	}
	m := FloorMeta{}
	var done int
	err = s.db.QueryRow(`SELECT next_url,done FROM floor_cache_meta WHERE row_id=?`, rowID).Scan(&m.NextURL, &done)
	if err == nil {
		m.Done = done != 0
	}
	return out, m, nil
}

// UpsertFloors upsert 一批楼层。keepName=true（XML 骨架）不覆盖已有的
// 文本名；keepName=false（HTML 逐页解析）覆盖 name，但已有楼层的图片和
// xml_title 保留（XML 通道优先）。
func (s *Store) UpsertFloors(rowID int64, rows []FloorRow, keepName bool) error {
	for _, r := range rows {
		var q string
		if keepName {
			q = `INSERT INTO floor_cache(row_id,post_id,no,name,xml_title,images) VALUES(?,?,?,?,?,?)
				ON CONFLICT(row_id,post_id) DO UPDATE SET
					no=excluded.no,
					xml_title=CASE WHEN excluded.xml_title!='' THEN excluded.xml_title ELSE floor_cache.xml_title END,
					images=CASE WHEN excluded.images!='[]' THEN excluded.images ELSE floor_cache.images END`
		} else {
			// HTML 逐页解析：文本名覆盖；解析出有效图片时也覆盖旧缓存
			//（老版本解析 bug 会把论坛按钮图标当图片存进来，必须能自愈）
			q = `INSERT INTO floor_cache(row_id,post_id,no,name,xml_title,images) VALUES(?,?,?,?,?,?)
				ON CONFLICT(row_id,post_id) DO UPDATE SET
					no=excluded.no,
					name=CASE WHEN excluded.name!='' THEN excluded.name ELSE floor_cache.name END,
					xml_title=CASE WHEN excluded.xml_title!='' THEN excluded.xml_title ELSE floor_cache.xml_title END,
					images=CASE WHEN excluded.images!='[]' THEN excluded.images ELSE floor_cache.images END`
		}
		if _, err := s.db.Exec(q, rowID, r.ForumPostID, r.FloorNo, r.Text, r.SourceTitle, r.ImagesJSON); err != nil {
			return fmt.Errorf("upsert floor %d: %w", r.ForumPostID, err)
		}
	}
	return nil
}

func (s *Store) SaveFloorMeta(rowID int64, m FloorMeta) error {
	_, err := s.db.Exec(`INSERT INTO floor_cache_meta(row_id,next_url,done) VALUES(?,?,?)
		ON CONFLICT(row_id) DO UPDATE SET next_url=excluded.next_url, done=excluded.done`,
		rowID, m.NextURL, boolInt(m.Done))
	return err
}

func (s *Store) DeleteFloorCache(rowID int64) error {
	if _, err := s.db.Exec(`DELETE FROM floor_cache WHERE row_id=?`, rowID); err != nil {
		return err
	}
	_, err := s.db.Exec(`DELETE FROM floor_cache_meta WHERE row_id=?`, rowID)
	return err
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}
