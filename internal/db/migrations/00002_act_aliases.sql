-- +goose Up
-- Seed aliases for the most cited acts. Aliases point to the ELI id of the
-- ORIGINAL act (Dz.U. year + position); consolidated texts are linked to it
-- through relations. Normalization rule (see internal/citation.NormalizeAlias):
-- lower case, dots removed, whitespace collapsed.
-- TODO(spike): verify every ELI id against api.sejm.gov.pl before relying on it.
INSERT INTO act_alias (alias, eli) VALUES
  ('konstytucja',                                             'DU/1997/483'),
  ('konstytucji',                                             'DU/1997/483'),
  ('konstytucja rp',                                          'DU/1997/483'),
  ('kc',                                                      'DU/1964/93'),
  ('kodeks cywilny',                                          'DU/1964/93'),
  ('kodeksu cywilnego',                                       'DU/1964/93'),
  ('kpc',                                                     'DU/1964/296'),
  ('kodeks postępowania cywilnego',                           'DU/1964/296'),
  ('kodeksu postępowania cywilnego',                          'DU/1964/296'),
  ('kk',                                                      'DU/1997/553'),
  ('kodeks karny',                                            'DU/1997/553'),
  ('kodeksu karnego',                                         'DU/1997/553'),
  ('kpa',                                                     'DU/1960/168'),
  ('kodeks postępowania administracyjnego',                   'DU/1960/168'),
  ('kodeksu postępowania administracyjnego',                  'DU/1960/168'),
  ('kp',                                                      'DU/1974/141'),
  ('kodeks pracy',                                            'DU/1974/141'),
  ('kodeksu pracy',                                           'DU/1974/141'),
  ('ksh',                                                     'DU/2000/1037'),
  ('kodeks spółek handlowych',                                'DU/2000/1037'),
  ('kodeksu spółek handlowych',                               'DU/2000/1037'),
  ('op',                                                      'DU/1997/926'),
  ('ordynacja podatkowa',                                     'DU/1997/926'),
  ('ordynacji podatkowej',                                    'DU/1997/926'),
  ('ustawa o vat',                                            'DU/2004/535'),
  ('ustawy o vat',                                            'DU/2004/535'),
  ('uptu',                                                    'DU/2004/535'),
  ('ustawa o podatku od towarów i usług',                     'DU/2004/535'),
  ('ustawy o podatku od towarów i usług',                     'DU/2004/535'),
  ('ustawa o cit',                                            'DU/1992/86'),
  ('ustawy o cit',                                            'DU/1992/86'),
  ('updop',                                                   'DU/1992/86'),
  ('ustawa o podatku dochodowym od osób prawnych',            'DU/1992/86'),
  ('ustawy o podatku dochodowym od osób prawnych',            'DU/1992/86'),
  ('ustawa o pit',                                            'DU/1991/350'),
  ('ustawy o pit',                                            'DU/1991/350'),
  ('updof',                                                   'DU/1991/350'),
  ('ustawa o podatku dochodowym od osób fizycznych',          'DU/1991/350'),
  ('ustawy o podatku dochodowym od osób fizycznych',          'DU/1991/350');

-- +goose Down
DELETE FROM act_alias;
