'use strict';
const {
  Model
} = require('sequelize');
module.exports = (sequelize, DataTypes) => {
  class HashLookup extends Model {
    /**
     * Helper method for defining associations.
     * This method is not a part of Sequelize lifecycle.
     * The `models/index` file will call this method automatically.
     */
    static associate(models) {
      // define association here
    }
  }
  HashLookup.init({
    txn_uuid: DataTypes.STRING,
    md5: DataTypes.STRING,
    contentType: DataTypes.STRING,
    filename: DataTypes.STRING,
    size: DataTypes.INTEGER,
    action: DataTypes.STRING
  }, {
    sequelize,
    modelName: 'HashLookup',
  });
  return HashLookup;
};